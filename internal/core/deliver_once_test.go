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
	c, err := claimDelivery(s, "x")
	if c == nil || err != nil {
		t.Fatalf("первый захват: %v", err)
	}
	if c2, err := claimDelivery(s, "x"); c2 != nil || err != nil {
		t.Fatalf("второй захват при идущей отправке: %v", err)
	}
	if got := deliver(ctx, s, "x", "test"); got != "" {
		t.Fatalf("второй путь отправил: %q", got)
	}
	var d *Denied
	if _, err := DeliverApproved(ctx, s, "x", "test"); err == nil || !errors.As(err, &d) {
		t.Fatalf("tg_send_draft при идущей отправке: %v", err)
	}
	c.release()
	if c, _ = claimDelivery(s, "x"); c == nil {
		t.Fatal("после конца отправки")
	}
	c.release()
}

// Черновик держит другой процесс (его лок-файл): этот не отправляет и
// карточку не трогает — итог пишет тот, кто отправляет.
func TestDeliverOtherProcess(t *testing.T) {
	s := watchEnv(t)
	ctx := context.Background()
	path := deliveryLockPath(s, "y")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	other := lock.New(path, 0)
	if err := other.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	if c, err := claimDelivery(s, "y"); c != nil || err != nil {
		t.Fatalf("захват при чужом лок-файле: %v", err)
	}
	if got := deliver(ctx, s, "y", "test"); got != "" {
		t.Fatalf("второй процесс отправил: %q", got)
	}
}

// Лок завести нельзя — не отправлять (иначе другой процесс мог бы отправить
// тот же черновик), а показать сбой.
func TestDeliverNoLockNoSend(t *testing.T) {
	s := watchEnv(t)
	box := outbox.New(s.OutboxPath())
	dr, err := box.Create(outbox.CreateOpts{Chat: "test", Text: "раз", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Approve(dr.ID, "test"); err != nil {
		t.Fatal(err)
	}
	// на месте каталога локов — файл
	if err := os.WriteFile(filepath.Join(s.DataDir(), "sending"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := deliver(context.Background(), s, dr.ID, "test"); !strings.Contains(got, "Не отправлено") {
		t.Fatalf("итог: %q", got)
	}
	if d, _ := box.Get(dr.ID); d.SendError == nil || d.Status != outbox.Approved {
		t.Fatalf("черновик: %+v", d)
	}
}

// Лок умершего процесса (упал посреди отправки) не мешает досылке взять
// черновик: снимается при первой попытке, берётся со следующей.
func TestClaimAfterDeadOwner(t *testing.T) {
	s := watchEnv(t)
	path := deliveryLockPath(s, "z")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".lock", []byte(`{"pid":999999,"ts":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := claimDelivery(s, "z")
	if c == nil || err != nil {
		t.Fatalf("лок умершего процесса не снят: %v", err)
	}
	c.release()
}
