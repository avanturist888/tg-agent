// Package tgc — пользовательская сессия Telegram (MTProto через gotd).
//
// Файл сессии один, а MCP-сервер поднимается в каждом проекте своим
// процессом. Поэтому соединение живёт только на время запроса и берётся под
// файловым локом: параллельные агенты не дерутся за сессию.
package tgc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/gotd/log/logslog"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"golang.org/x/net/proxy"

	"tgagent/internal/audit"
	"tgagent/internal/config"
	"tgagent/internal/lock"
)

// NotLoggedIn — нет валидной сессии, нужен tg login.
type NotLoggedIn struct{ Msg string }

func (e *NotLoggedIn) Error() string { return e.Msg }

// Opts — как занимать сессию.
type Opts struct {
	// Wait — сколько ждать, пока сессию отпустит другой процесс (0 — 45 с).
	Wait time.Duration
	// Background — фоновая работа (опрос лент): уступает всем. Если сессия
	// занята — сразу lock.Busy; если держим, а сессия понадобилась кому-то
	// ещё в этом процессе (отправка после кнопки), фоновую работу отменяют.
	Background bool
	// NoAuth — не требовать авторизации (login/logout).
	NoAuth bool
	// Caller — кто держит сессию, для журнала долгих захватов.
	Caller string
}

// Conn — открытое соединение на время одного запроса.
type Conn struct {
	Client   *telegram.Client
	API      *tg.Client
	Settings *config.Settings
	Peers    *PeerCache

	hub *updateHub
}

var (
	procMu   sync.Mutex
	procSem  = make(chan struct{}, 1) // сессия одна на весь процесс
	bgCancel context.CancelFunc       // отмена текущей фоновой работы
)

func acquireProc(ctx context.Context, o Opts, wait time.Duration) (context.Context, func(), error) {
	if o.Background {
		select {
		case procSem <- struct{}{}:
		default:
			return nil, nil, &lock.Busy{Msg: "Сессия Telegram занята — фоновая задача подождёт."}
		}
		bctx, cancel := context.WithCancel(ctx)
		procMu.Lock()
		bgCancel = cancel
		procMu.Unlock()
		return bctx, func() {
			procMu.Lock()
			bgCancel = nil
			procMu.Unlock()
			cancel()
			<-procSem
		}, nil
	}
	procMu.Lock()
	if bgCancel != nil {
		bgCancel() // опрос лент повторится на следующем круге, отправка — нет
	}
	procMu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case procSem <- struct{}{}:
		return ctx, func() { <-procSem }, nil
	case <-timer.C:
		return nil, nil, &lock.Busy{Msg: "Сессия Telegram занята другой задачей этого процесса."}
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func callerName(o Opts) string {
	if o.Caller != "" {
		return o.Caller
	}
	pc, _, _, ok := runtime.Caller(2)
	if !ok {
		return "?"
	}
	if fn := runtime.FuncForPC(pc); fn != nil {
		return fn.Name()
	}
	return "?"
}

// Run — подключиться, выполнить fn и сразу отпустить сессию.
func Run(ctx context.Context, s *config.Settings, o Opts, fn func(ctx context.Context, c *Conn) error) error {
	wait := o.Wait
	if wait == 0 {
		wait = 45 * time.Second
	}
	caller := callerName(o)
	ctx, release, err := acquireProc(ctx, o, wait)
	if err != nil {
		return err
	}
	defer release()

	flock := lock.New(s.SessionPath, wait)
	if o.Background {
		flock.Timeout = 500 * time.Millisecond
	}
	if err := flock.Acquire(ctx); err != nil {
		return err
	}
	taken := time.Now()
	defer func() {
		flock.Release()
		if held := time.Since(taken); held > 30*time.Second {
			// долгие захваты мешают отправке — пусть будет видно, кто
			_ = audit.Log(s.AuditPath(), "long_session_hold", "seconds", int(held.Round(time.Second).Seconds()), "by", caller)
		}
	}()

	c := &Conn{Settings: s, hub: newUpdateHub()}
	client, err := newClient(s, c.hub)
	if err != nil {
		return err
	}
	c.Client = client
	// ошибку своего кода отдаём как есть, без обёрток gotd («callback: …»)
	var ferr error
	rerr := client.Run(ctx, func(ctx context.Context) error {
		ferr = c.start(ctx, client, s, o, fn)
		return ferr
	})
	if ferr != nil {
		return ferr
	}
	return rerr
}

func (c *Conn) start(ctx context.Context, client *telegram.Client, s *config.Settings, o Opts, fn func(ctx context.Context, c *Conn) error) error {
	c.API = client.API()
	if !o.NoAuth {
		st, err := client.Auth().Status(ctx)
		if err != nil {
			return err
		}
		if !st.Authorized {
			return &NotLoggedIn{fmt.Sprintf("Нет валидной сессии Telegram. Человек должен выполнить "+
				"`%s login` и ввести код из Telegram.", config.CLIHint())}
		}
	}
	c.Peers = LoadPeers(s.PeersPath())
	return fn(ctx, c)
}

func newClient(s *config.Settings, hub *updateHub) (*telegram.Client, error) {
	opts := telegram.Options{
		SessionStorage: &session.FileStorage{Path: s.SessionPath},
		Device: telegram.DeviceConfig{
			DeviceModel:   "tg-agent",
			SystemVersion: "claude-code",
			AppVersion:    "0.2.0",
		},
		UpdateHandler: hub,
		DialTimeout:   20 * time.Second,
		Clock:         skewClock{offset: ClockOffset(s)},
	}
	if os.Getenv("TG_GO_DEBUG") != "" {
		// подробный журнал MTProto в stderr — только для отладки подключения
		opts.Logger = logslog.New(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	}
	if s.Proxy != nil {
		dialer, err := proxy.FromURL(s.Proxy, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("TG_PROXY: %w", err)
		}
		ctxDialer, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, errors.New("TG_PROXY: прокси не поддерживает контекст")
		}
		opts.Resolver = dcs.Plain(dcs.PlainOptions{Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return ctxDialer.DialContext(ctx, network, addr)
		}})
	}
	return telegram.NewClient(s.APIID, s.APIHash, opts), nil
}

// NewLoginClient — клиент для интерактивного входа (tg login), без лока.
func NewLoginClient(s *config.Settings) (*telegram.Client, error) {
	return newClient(s, newUpdateHub())
}

// updateHub раздаёт апдейты тем, кто их ждёт (расшифровка голосовых).
type updateHub struct {
	mu          sync.Mutex
	transcribed map[int]chan *tg.UpdateTranscribedAudio
}

func newUpdateHub() *updateHub {
	return &updateHub{transcribed: map[int]chan *tg.UpdateTranscribedAudio{}}
}

func (h *updateHub) Handle(_ context.Context, u tg.UpdatesClass) error {
	var list []tg.UpdateClass
	switch v := u.(type) {
	case *tg.Updates:
		list = v.Updates
	case *tg.UpdatesCombined:
		list = v.Updates
	case *tg.UpdateShort:
		list = []tg.UpdateClass{v.Update}
	}
	for _, up := range list {
		if t, ok := up.(*tg.UpdateTranscribedAudio); ok {
			h.mu.Lock()
			ch := h.transcribed[t.MsgID]
			h.mu.Unlock()
			if ch != nil {
				select {
				case ch <- t:
				default:
				}
			}
		}
	}
	return nil
}

func (h *updateHub) watchTranscribed(msgID int) (<-chan *tg.UpdateTranscribedAudio, func()) {
	ch := make(chan *tg.UpdateTranscribedAudio, 16)
	h.mu.Lock()
	h.transcribed[msgID] = ch
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.transcribed, msgID)
		h.mu.Unlock()
	}
}

func parseURL(raw string) (*url.URL, error) { return url.Parse(raw) }
