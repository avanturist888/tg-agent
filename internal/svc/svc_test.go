package svc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"tgagent/internal/config"
)

func TestRoundTripErrorsAndCancel(t *testing.T) {
	old := config.Root
	config.Root = t.TempDir() // свой канал, не мешать настоящей службе
	os.MkdirAll(config.Root+"/data", 0o700)
	t.Cleanup(func() { config.Root = old })

	cancelled := make(chan struct{})
	srv, err := Listen(map[string]Handler{
		"echo": func(ctx context.Context, args json.RawMessage) (any, error) {
			var v map[string]any
			json.Unmarshal(args, &v)
			return v, nil
		},
		"fail": func(context.Context, json.RawMessage) (any, error) { return nil, errors.New("нельзя") },
		"slow": func(ctx context.Context, _ json.RawMessage) (any, error) {
			<-ctx.Done()
			close(cancelled)
			return nil, ctx.Err()
		},
		"panic": func(context.Context, json.RawMessage) (any, error) { panic("бум") },
	}, func(error) string { return "denied" })
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go srv.Serve(ctx)

	if !Up(ctx) {
		t.Fatal("служба не отвечает")
	}
	if _, err := Listen(nil, nil); err == nil {
		t.Fatal("второй экземпляр занял канал")
	}
	var out map[string]string
	if _, err := Call(ctx, "echo", map[string]string{"привет": "мир"}, &out); err != nil || out["привет"] != "мир" {
		t.Fatalf("echo: %v %v", err, out)
	}
	_, err = Call(ctx, "fail", nil, nil)
	var e *Error
	if !errors.As(err, &e) || e.Kind != "denied" || e.Message != "нельзя" {
		t.Fatalf("fail: %v", err)
	}
	if _, err := Call(ctx, "panic", nil, nil); err == nil {
		t.Fatal("паника не превратилась в ошибку")
	}
	if _, err := Call(ctx, "nope", nil, nil); err == nil {
		t.Fatal("неизвестная операция прошла")
	}
	cctx, ccancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer ccancel()
	if _, err := Call(cctx, "slow", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow: %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("отмена клиента не дошла до операции")
	}
	stop()
	time.Sleep(200 * time.Millisecond)
	if _, err := Call(context.Background(), "echo", nil, nil); !errors.Is(err, Unavailable) {
		t.Fatalf("после остановки: %v", err)
	}
}
