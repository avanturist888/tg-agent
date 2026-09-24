// Package service — служба tg-agent и её операции.
//
// Служба (`tg serve`, в Планировщике — `tgw.exe serve`) держит постоянное
// соединение с Telegram, пишет ленты по событиям, разбирает кнопки бота и
// досылает автоотправку. Остальные процессы — MCP-прослойки агентов, CLI,
// окно управления — вызывают её операции через Do. Если служба не запущена,
// Do выполняет ту же операцию на месте, с прямым подключением к Telegram.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"tgagent/internal/audit"
	"tgagent/internal/bot"
	"tgagent/internal/config"
	"tgagent/internal/core"
	"tgagent/internal/lock"
	"tgagent/internal/mcpserver"
	"tgagent/internal/svc"
	"tgagent/internal/tgc"
)

// ── аргументы и результаты операций ─────────────────────────────────────

type ToolArgs struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type LimitArgs struct {
	Limit int `json:"limit"`
}

type DraftArgs struct {
	ID string `json:"id"`
	By string `json:"by"`
}

type ValueArgs struct {
	Value string `json:"value"`
}

// Self — владелец аккаунта.
type Self struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

// Status — состояние службы.
type Status struct {
	Build      string    `json:"build"`
	PID        int       `json:"pid"`
	Started    time.Time `json:"started"`
	Connected  bool      `json:"connected"`
	Authorized bool      `json:"authorized"`
	Buttons    bool      `json:"buttons"` // слушает кнопки бота
}

// Check — живая проверка для doctor и окна.
type Check struct {
	Session      string  `json:"session"` // ok / not_logged_in / error
	SessionError string  `json:"session_error,omitempty"`
	User         *Self   `json:"user,omitempty"`
	Bot          string  `json:"bot,omitempty"`
	BotError     string  `json:"bot_error,omitempty"`
	ClockOffset  float64 `json:"clock_offset_sec"`
}

// ── операции ─────────────────────────────────────────────────────────────

type op struct {
	fn func(ctx context.Context, args json.RawMessage) (any, error)
	// local — можно выполнить на месте, если службы нет
	local bool
}

var started = time.Now()

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) > 0 && string(raw) != "null" {
		err := json.Unmarshal(raw, &v)
		return v, err
	}
	return v, nil
}

func withSettings[T any](fn func(ctx context.Context, s *config.Settings, a T) (any, error)) func(context.Context, json.RawMessage) (any, error) {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		a, err := decode[T](raw)
		if err != nil {
			return nil, &core.Bad{Msg: "аргументы: " + err.Error()}
		}
		s, err := config.Load()
		if err != nil {
			return nil, err
		}
		return fn(ctx, s, a)
	}
}

var ops map[string]op

func init() {
	ops = map[string]op{
		"tools": {local: true, fn: func(ctx context.Context, _ json.RawMessage) (any, error) {
			return mcpserver.Tools(ctx)
		}},
		"tool": {local: true, fn: func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := decode[ToolArgs](raw)
			if err != nil {
				return nil, &core.Bad{Msg: "аргументы: " + err.Error()}
			}
			return mcpserver.CallTool(ctx, a.Name, a.Args)
		}},
		"self": {local: true, fn: withSettings(func(ctx context.Context, s *config.Settings, _ struct{}) (any, error) {
			var me Self
			err := tgc.Run(ctx, s, tgc.Opts{}, func(ctx context.Context, c *tgc.Conn) error {
				u, err := c.Self(ctx)
				if err != nil {
					return err
				}
				c.Peers.Save()
				me = Self{ID: u.ID, FirstName: u.FirstName, LastName: u.LastName, Username: u.Username}
				return nil
			})
			return me, err
		})},
		"dialogs": {local: true, fn: withSettings(func(ctx context.Context, s *config.Settings, a LimitArgs) (any, error) {
			var rows []tgc.DialogRow
			err := tgc.Run(ctx, s, tgc.Opts{}, func(ctx context.Context, c *tgc.Conn) error {
				var err error
				rows, err = c.ListDialogs(ctx, a.Limit)
				return err
			})
			return rows, err
		})},
		"approve_send": {local: true, fn: withSettings(func(ctx context.Context, s *config.Settings, a DraftArgs) (any, error) {
			summary, err := core.ApproveAndSend(ctx, s, a.ID, a.By)
			return map[string]string{"summary": summary}, err
		})},
		"poll": {local: true, fn: withSettings(func(ctx context.Context, s *config.Settings, _ struct{}) (any, error) {
			return core.PollOnce(ctx, s)
		})},
		"check": {local: true, fn: withSettings(func(ctx context.Context, s *config.Settings, _ struct{}) (any, error) {
			return check(ctx, s), nil
		})},
		"status": {fn: func(ctx context.Context, _ json.RawMessage) (any, error) {
			connected, authorized := tgc.ServiceAuthorized()
			return Status{Build: config.Build, PID: os.Getpid(), Started: started, Connected: connected,
				Authorized: authorized, Buttons: buttonsActive.Load()}, nil
		}},
		"login_begin": {fn: withSettings(func(ctx context.Context, s *config.Settings, a ValueArgs) (any, error) {
			phone := a.Value
			if phone == "" {
				phone = s.Phone
			}
			if phone == "" {
				return nil, &core.Bad{Msg: "нужен номер телефона"}
			}
			return tgc.ServiceLoginBegin(ctx, phone)
		})},
		"login_answer": {fn: withSettings(func(ctx context.Context, s *config.Settings, a ValueArgs) (any, error) {
			step, err := tgc.ServiceLoginAnswer(ctx, a.Value)
			if err == nil && step.Stage == "done" {
				_ = audit.Log(s.AuditPath(), "login", "user", step.User)
			}
			return step, err
		})},
		"logout": {fn: withSettings(func(ctx context.Context, s *config.Settings, _ struct{}) (any, error) {
			if err := tgc.ServiceLogout(ctx); err != nil {
				return nil, err
			}
			_ = audit.Log(s.AuditPath(), "logout")
			return map[string]bool{"ok": true}, nil
		})},
	}
}

func check(ctx context.Context, s *config.Settings) Check {
	var out Check
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	err := tgc.Run(ctx, s, tgc.Opts{Wait: 20 * time.Second}, func(ctx context.Context, c *tgc.Conn) error {
		u, err := c.Self(ctx)
		if err != nil {
			return err
		}
		c.Peers.Save()
		out.User = &Self{ID: u.ID, FirstName: u.FirstName, LastName: u.LastName, Username: u.Username}
		return nil
	})
	var nl *tgc.NotLoggedIn
	switch {
	case err == nil:
		out.Session = "ok"
	case errors.As(err, &nl):
		out.Session = "not_logged_in"
	default:
		out.Session, out.SessionError = "error", err.Error()
	}
	if s.BotReady() {
		if name, err := bot.Me(ctx, s); err == nil {
			out.Bot = "@" + name
		} else {
			out.BotError = err.Error()
		}
	}
	out.ClockOffset = tgc.ClockOffset(s).Seconds()
	return out
}

func handlers() map[string]svc.Handler {
	out := map[string]svc.Handler{}
	for name, o := range ops {
		out[name] = o.fn
	}
	return out
}

// ── вызов ────────────────────────────────────────────────────────────────

// Do — выполнить операцию в службе, а если её нет — на месте (если операция
// это позволяет). Ошибки возвращаются тех же типов, что и на месте.
func Do(ctx context.Context, name string, args, out any) error {
	_, err := DoBuild(ctx, name, args, out)
	return err
}

// DoBuild — то же, плюс сборка службы ("" — выполнено на месте).
func DoBuild(ctx context.Context, name string, args, out any) (string, error) {
	build, err := svc.Call(ctx, name, args, out)
	if !errors.Is(err, svc.Unavailable) {
		return build, typed(err)
	}
	o, ok := ops[name]
	if !ok || !o.local {
		return "", svc.Unavailable
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	res, err := o.fn(ctx, raw)
	if err != nil {
		return "", err
	}
	if out != nil {
		// через JSON — те же типы, что и из службы
		b, err := json.Marshal(res)
		if err != nil {
			return "", err
		}
		return "", json.Unmarshal(b, out)
	}
	return "", nil
}

// Up — запущена ли служба.
func Up(ctx context.Context) bool { return svc.Up(ctx) }

// typed — ошибка из службы в тех же типах, что и на месте.
func typed(err error) error {
	var e *svc.Error
	if !errors.As(err, &e) {
		return err
	}
	switch e.Kind {
	case "not_logged_in":
		return &tgc.NotLoggedIn{Msg: e.Message}
	case "denied":
		return &core.Denied{Msg: e.Message}
	case "bad_request", "not_found":
		return &core.Bad{Msg: e.Message}
	case "SessionBusy":
		return &lock.Busy{Msg: e.Message}
	case "config":
		return &config.Error{Msg: e.Message}
	}
	return err
}

// ToolResult — результат инструмента, как его отдаёт служба.
type ToolResult = mcp.CallToolResult
