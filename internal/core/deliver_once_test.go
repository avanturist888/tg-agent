package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tgagent/internal/lock"
	"tgagent/internal/outbox"
)

// 12.11.2026 черновик ушёл дважды: после перезапуска службы автоотправка
// одобрила его, а досылка «прерванного прошлой службой» тут же отправила
// его ещё раз. Одобренное этим процессом досылка не трогает, а один
// черновик два пути не отправляют.
func TestRecoverSkipsOwnApproved(t *testing.T) {
	s := watchEnv(t)
	s.SendPolicy = "bot_approval"
	box := outbox.New(s.OutboxPath())
	d, err := box.Create(outbox.CreateOpts{Chat: "test", Text: "раз", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if d, err = box.Approve(d.ID, "auto_send"); err != nil {
		t.Fatal(err)
	}
	was := processStarted
	processStarted = time.Now().Add(-time.Hour) // служба запущена до одобрения
	t.Cleanup(func() { processStarted = was })
	RecoverInterrupted(context.Background(), s)
	if raw, _ := os.ReadFile(s.AuditPath()); strings.Contains(string(raw), "recover_interrupted") {
		t.Fatal("досылка взяла черновик, одобренный этим же процессом")
	}
	old := "2026-11-01T12:00:00+00:00"
	if !approvedBefore(&outbox.Draft{ApprovedAt: &old}, processStarted) || approvedBefore(d, processStarted.Add(-time.Hour)) {
		t.Fatal("approvedBefore")
	}
}

func TestDeliverOnce(t *testing.T) {
	s := watchEnv(t)
	ctx := context.Background()
	c := claimDelivery(s, "x")
	if c == nil {
		t.Fatal("первый захват")
	}
	if claimDelivery(s, "x") != nil {
		t.Fatal("второй захват при идущей отправке")
	}
	if got := deliverWith(ctx, s, "x", "test", false); got != "" {
		t.Fatalf("второй путь отправил: %q", got)
	}
	var d *Denied
	if _, err := DeliverApproved(ctx, s, "x", "test"); err == nil || !errors.As(err, &d) {
		t.Fatalf("tg_send_draft при идущей отправке: %v", err)
	}
	c.release()
	if c = claimDelivery(s, "x"); c == nil {
		t.Fatal("после конца отправки")
	}
	c.release()
}

// Черновик отправляет другой процесс (его лок-файл): этот не отправляет, а
// проигравший путь с ожиданием получает итог победителя.
func TestDeliverOtherProcess(t *testing.T) {
	s := watchEnv(t)
	ctx := context.Background()
	box := outbox.New(s.OutboxPath())
	dr, err := box.Create(outbox.CreateOpts{Chat: "test", Text: "раз", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Approve(dr.ID, "test"); err != nil {
		t.Fatal(err)
	}
	path := deliveryLockPath(s, dr.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	other := lock.New(path, 0) // «другой процесс» держит черновик
	if err := other.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if claimDelivery(s, dr.ID) != nil {
		t.Fatal("захват при чужом лок-файле")
	}
	go func() {
		time.Sleep(700 * time.Millisecond)
		_, _ = box.MarkSent(dr.ID, 777, "2026-11-12T13:00:00+00:00")
		other.Release()
	}()
	if got := deliver(ctx, s, dr.ID, "test"); !strings.Contains(got, "Отправлено") || !strings.Contains(got, "777") {
		t.Fatalf("итог победителя: %q", got)
	}
}
