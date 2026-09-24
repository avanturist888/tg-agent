// Package svc — канал между службой tg-agent и остальными процессами.
//
// Служба (`tg serve`) держит единственное постоянное соединение с Telegram.
// MCP-прослойки агентов, CLI и окно управления вызывают её операции через
// локальный канал: named pipe на Windows (доступ только у текущей учётной
// записи), unix-сокет в data/ на остальных системах.
//
// Протокол: одно соединение — один запрос. Клиент пишет строку JSON
// {"op", "args"}, служба отвечает строкой {"result"} или {"error"}. Разрыв
// соединения клиентом отменяет операцию на стороне службы.
package svc

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"tgagent/internal/config"
)

// Handler — операция службы.
type Handler func(ctx context.Context, args json.RawMessage) (any, error)

// Error — ошибка операции в виде, пригодном для передачи по каналу.
type Error struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// Unavailable — служба не запущена или не отвечает.
var Unavailable = errors.New("служба tg-agent не запущена")

type request struct {
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

type response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
	Build  string          `json:"build"`
}

// Address — имя канала: своё для каждой установки (по пути к ней).
func Address() string {
	sum := sha256.Sum256([]byte(strings.ToLower(config.Root)))
	return address(hex.EncodeToString(sum[:6]))
}

// Classify — как ошибку назвать на другой стороне канала.
type Classify func(error) string

// Server — приём запросов службы.
type Server struct {
	handlers map[string]Handler
	classify Classify
	ln       net.Listener
	wg       sync.WaitGroup
}

// Listen занимает канал. Если его держит другая служба — ошибка: второй
// экземпляр не нужен.
func Listen(handlers map[string]Handler, classify Classify) (*Server, error) {
	ln, err := listen(Address())
	if err != nil {
		return nil, fmt.Errorf("канал службы занят (служба уже запущена?): %w", err)
	}
	return &Server{handlers: handlers, classify: classify, ln: ln}, nil
}

// Serve обслуживает запросы, пока не отменят ctx.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = s.ln.Close()
	}()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.wg.Wait()
				return nil
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(ctx, conn)
		}()
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	rd := bufio.NewReader(conn)
	line, err := rd.ReadBytes('\n')
	if err != nil {
		return
	}
	var req request
	resp := response{Build: config.Build}
	if err := json.Unmarshal(line, &req); err != nil {
		resp.Error = &Error{Kind: "bad_request", Message: "запрос не разобрался: " + err.Error()}
		writeLine(conn, resp)
		return
	}
	h, ok := s.handlers[req.Op]
	if !ok {
		resp.Error = &Error{Kind: "bad_request", Message: "служба не знает операцию " + req.Op}
		writeLine(conn, resp)
		return
	}
	// клиент ушёл (агент отменил вызов) — операцию прекращаем
	opCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, rd)
		cancel()
	}()
	result, err := safeCall(opCtx, h, req.Args)
	if err != nil {
		resp.Error = &Error{Kind: s.classify(err), Message: err.Error()}
	} else if raw, merr := json.Marshal(result); merr != nil {
		resp.Error = &Error{Kind: "error", Message: merr.Error()}
	} else {
		resp.Result = raw
	}
	writeLine(conn, resp)
}

func safeCall(ctx context.Context, h Handler, args json.RawMessage) (out any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("внутренняя ошибка службы: %v", r)
		}
	}()
	return h(ctx, args)
}

func writeLine(w io.Writer, v any) {
	raw, _ := json.Marshal(v)
	_, _ = w.Write(append(raw, '\n'))
}

// ── клиент ───────────────────────────────────────────────────────────────

// Up — отвечает ли служба (быстрая проверка).
func Up(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	conn, err := dial(ctx, Address())
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Call выполняет операцию в службе. out — куда разобрать результат (может быть nil).
// Возвращает сборку службы (для прослойки: сменилась — перечитать инструменты).
func Call(ctx context.Context, op string, args, out any) (string, error) {
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	conn, err := dial(dctx, Address())
	cancel()
	if err != nil {
		return "", Unavailable
	}
	defer conn.Close()
	// отмена вызова — рвём соединение, служба прекратит операцию
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	req := request{Op: op}
	if args != nil {
		raw, err := json.Marshal(args)
		if err != nil {
			return "", err
		}
		req.Args = raw
	}
	writeLine(conn, req)
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("служба оборвала соединение: %w", err)
	}
	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		return "", fmt.Errorf("ответ службы не разобрался: %w", err)
	}
	if resp.Error != nil {
		return resp.Build, resp.Error
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return resp.Build, fmt.Errorf("результат службы не разобрался: %w", err)
		}
	}
	return resp.Build, nil
}
