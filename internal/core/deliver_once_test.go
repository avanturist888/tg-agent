package core

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

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
	if !claimDelivery("x") {
		t.Fatal("первый захват")
	}
	if claimDelivery("x") {
		t.Fatal("второй захват при идущей отправке")
	}
	if got := deliver(context.Background(), s, "x", "test"); got != "" {
		t.Fatalf("второй путь отправил: %q", got)
	}
	var d *Denied
	if _, err := DeliverApproved(context.Background(), s, "x", "test"); err == nil || !errors.As(err, &d) {
		t.Fatalf("tg_send_draft при идущей отправке: %v", err)
	}
	releaseDelivery("x")
	if !claimDelivery("x") {
		t.Fatal("после конца отправки")
	}
	releaseDelivery("x")
}
