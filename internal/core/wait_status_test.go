package core

import (
	"context"
	"testing"
	"time"

	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/svc"
)

// Only a temporary installation's pipe is opened; it has no delivery handler.
func waitStatusService(t *testing.T) {
	t.Helper()
	srv, err := svc.Listen(map[string]svc.Handler{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func TestWaitStatusDoesNotJoinSendingBatch(t *testing.T) {
	s := watchEnv(t)
	waitStatusService(t)
	at := time.Now().Add(time.Hour)
	d, err := outbox.New(s.DataDir()).Create(outbox.CreateOpts{Chat: "test", Text: "synthetic", TTLMin: 120, SendAt: &at})
	if err != nil {
		t.Fatal(err)
	}
	// Hold the exact mutex held by a large upload batch. No file or network
	// upload is needed, and the observer must finish while it remains held.
	sendingDue.Lock()
	locked := true
	defer func() {
		if locked {
			sendingDue.Unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	type result struct {
		out *omap.Map
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := WaitForDecision(ctx, s, d.ID, 75*time.Millisecond)
		done <- result{out, err}
	}()
	select {
	case got := <-done:
		if got.err != nil || got.out == nil || ctx.Err() != nil {
			t.Fatalf("observer: %v, %v", got.err, ctx.Err())
		}
	case <-time.After(time.Second):
		cancel()
		sendingDue.Unlock()
		locked = false
		<-done // join before temporary settings and service are removed
		t.Fatal("status observer waited for the upload batch")
	}
	current, err := outbox.New(s.DataDir()).Get(d.ID)
	if err != nil || current.Status != outbox.Scheduled || current.SendAt == nil || current.SendAt.Unix() != at.Unix() {
		t.Fatalf("observer changed draft: %+v, %v", current, err)
	}
}

func TestAutoApprovalHonorsExplicitWaitTimeout(t *testing.T) {
	s := watchEnv(t)
	s.ApprovalTimeoutSec, s.AutoSendDelaySec = 300, 10
	waitStatusService(t)
	at := time.Now().Add(time.Hour)
	d, err := outbox.New(s.DataDir()).Create(outbox.CreateOpts{Chat: "test", Text: "synthetic", TTLMin: 120, SendAt: &at})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	out, err := WaitApproval(ctx, s, d.ID, 1)
	if err != nil || out == nil || ctx.Err() != nil {
		t.Fatalf("explicit one-second wait was extended: %v, %v", err, ctx.Err())
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatal("wait returned before its observation period")
	}
}
