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
	box := outbox.New(s.DataDir())
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

func approved(t *testing.T, box *outbox.Outbox, chat string) *outbox.Draft {
	t.Helper()
	d, err := box.Create(outbox.CreateOpts{Chat: chat, Text: "раз", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if d, err = box.Approve(d.ID, "test"); err != nil {
		t.Fatal(err)
	}
	return d
}

// Черновик уже отправляет другой путь: второй не отправляет и карточку не
// трогает (итог напишет первый), tg_send_draft отказывает.
func TestDeliverOnce(t *testing.T) {
	s := watchEnv(t)
	ctx := context.Background()
	box := outbox.New(s.DataDir())
	d := approved(t, box, "test")
	c, _, err := box.ClaimSend(d.ID)
	if c == nil || err != nil {
		t.Fatalf("первый захват: %v", err)
	}
	if got := deliver(ctx, s, d.ID, "test"); got != "" {
		t.Fatalf("второй путь отправил: %q", got)
	}
	var dn *Denied
	if _, err := DeliverApproved(ctx, s, d.ID, "test"); err == nil || !errors.As(err, &dn) {
		t.Fatalf("tg_send_draft при идущей отправке: %v", err)
	}
	if got, _ := box.Get(d.ID); got.SendError != nil {
		t.Fatalf("отказ второму пути записан ошибкой черновика: %v", *got.SendError)
	}
	c.Release()
}

// Уже решённый черновик (его отправил другой путь) не отправляется снова, а
// итог — по его статусу.
func TestDeliverDecided(t *testing.T) {
	s := watchEnv(t)
	box := outbox.New(s.DataDir())
	d := approved(t, box, "test")
	if _, err := box.MarkSent(d.ID, 77, "2026-11-12T10:00:00+00:00"); err != nil {
		t.Fatal(err)
	}
	if got := deliver(context.Background(), s, d.ID, "test"); !strings.Contains(got, "id 77") {
		t.Fatalf("итог: %q", got)
	}
}

// Отказ после захвата (здесь — чат закрыт для отправки) записывается в
// черновик: иначе tg_wait_approval и карточка так и висели бы на
// «Отправляю…».
func TestDeliverApprovedDeniedRecorded(t *testing.T) {
	s := watchEnv(t)
	box := outbox.New(s.DataDir())
	d := approved(t, box, "closed-chat")
	if _, err := DeliverApproved(context.Background(), s, d.ID, "test"); err == nil {
		t.Fatal("отправка в закрытый чат прошла")
	}
	got, _ := box.Get(d.ID)
	if got.SendError == nil || got.Status != outbox.Approved {
		t.Fatalf("черновик: %+v", got)
	}
}
