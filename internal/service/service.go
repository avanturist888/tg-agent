package service

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"tgagent/internal/audit"
	"tgagent/internal/bot"
	"tgagent/internal/config"
	"tgagent/internal/core"
	"tgagent/internal/mcpserver"
	"tgagent/internal/svc"
	"tgagent/internal/tgc"
)

// ReconcileEvery — как часто сверять ленты с верхними сообщениями чатов:
// страховка от пропущенных событий, основная доставка — по событиям.
const ReconcileEvery = time.Minute

var buttonsActive atomic.Bool

// Run — служба: работает, пока не отменят ctx.
func Run(ctx context.Context, loader func() (*config.Settings, error)) error {
	s, err := loader()
	if err != nil {
		return err
	}
	core.RaisePriority()
	tgc.EnableService() // до приёма запросов: они пойдут через общее соединение
	srv, err := svc.Listen(handlers(), mcpserver.ErrorKind)
	if err != nil {
		return err
	}
	_ = audit.Log(s.AuditPath(), "service_start", "build", config.Build)
	fmt.Printf("Служба tg-agent (%s) запущена. Ctrl+C — выход.\n", config.Build)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{}, 6)
	go func() {
		defer func() { done <- struct{}{} }()
		_ = srv.Serve(ctx)
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		core.RunAutoSend(ctx, loader) // автоотправка в срок, независимо от бота
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		core.RunNotifier(ctx, loader) // будить подписанные сессии агентов
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		runHTTP(ctx, loader) // MCP по HTTP для агентов из контейнеров
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		if err := tgc.Serve(ctx, s, tgc.Events{Message: core.FeedOnMessage(loader),
			Edit: core.FeedOnEdit(loader), Delete: core.FeedOnDelete(loader)}, func(ctx context.Context, _ *tgc.Conn) {
			reconcile(ctx, loader) // догнать пропущенное, пока нас не было
		}, func(ctx context.Context) {
			noLogin(ctx, loader)
		}); err != nil {
			_ = audit.Log(s.AuditPath(), "service_failed", "error", err.Error())
			cancel()
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		for ctx.Err() == nil {
			sleep(ctx, ReconcileEvery)
			reconcile(ctx, loader)
		}
	}()
	// у общего бота нажатия разбирает основная служба и передаёт нам (op callback)
	if s.BotReady() && !s.SharedBot {
		buttonsActive.Store(true)
		err = core.RunButtons(ctx, s, loader)
		buttonsActive.Store(false)
		if err != nil && !errors.Is(err, context.Canceled) {
			_ = audit.Log(s.AuditPath(), "buttons_failed", "error", err.Error())
		}
	} else if s.SharedBot {
		core.RecoverInterrupted(ctx, s) // у своего бота это делает RunButtons
	}
	<-ctx.Done()
	for range 6 {
		<-done
	}
	_ = audit.Log(s.AuditPath(), "service_stop")
	return nil
}

func reconcile(ctx context.Context, loader func() (*config.Settings, error)) {
	s, err := loader()
	if err != nil {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := core.PollOnce(pctx, s); err != nil && ctx.Err() == nil {
		var nl *tgc.NotLoggedIn
		if !errors.As(err, &nl) {
			_ = audit.Log(s.AuditPath(), "feed_reconcile_failed", "error", err.Error())
		}
	}
}

// noLogin — Telegram не принимает сессию: агенты остались без доступа, а
// узнать об этом владелец иначе мог только от них.
func noLogin(ctx context.Context, loader func() (*config.Settings, error)) {
	s, err := loader()
	if err != nil {
		return
	}
	_ = audit.Log(s.AuditPath(), "service_not_logged_in")
	if !s.BotReady() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	who, login := "", "`tg login`"
	if config.Multi() {
		who = " (аккаунт " + config.AccountName() + ")"
	}
	if config.Profile() != "" {
		login = "`tg --profile " + config.Profile() + " login`"
	}
	err = bot.Call(ctx, s, "sendMessage", map[string]any{
		"chat_id": s.ApprovalChatID,
		"text": "⚠️ tg-agent" + who + ": Telegram не принимает сессию — у агентов нет доступа к чатам.\n" +
			"Войди заново: ярлык tg-agent → вкладка «Состояние» (или " + login + ").",
	}, nil)
	if err != nil {
		_ = audit.Log(s.AuditPath(), "notify_failed", "error", err.Error())
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
