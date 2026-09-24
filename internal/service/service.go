package service

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"tgagent/internal/audit"
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
	done := make(chan struct{}, 3)
	go func() {
		defer func() { done <- struct{}{} }()
		_ = srv.Serve(ctx)
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		if err := tgc.Serve(ctx, s, core.FeedOnMessage(loader), func(ctx context.Context, _ *tgc.Conn) {
			reconcile(ctx, loader) // догнать пропущенное, пока нас не было
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
	if s.BotReady() {
		buttonsActive.Store(true)
		err = core.RunButtons(ctx, s, loader)
		buttonsActive.Store(false)
		if err != nil && !errors.Is(err, context.Canceled) {
			_ = audit.Log(s.AuditPath(), "buttons_failed", "error", err.Error())
		}
	}
	<-ctx.Done()
	for range 3 {
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

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
