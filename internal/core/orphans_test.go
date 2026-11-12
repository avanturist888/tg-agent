package core

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/outbox"
)

// Процесс записал запрос доступа и умер, не отправив карточку: запрос не
// висит без кнопок — карточку шлёт следующий (повторный запрос агента или
// уборка службы), и только один. Пока отправляет живой — не трогаем.
func TestAccessOrphanAdopted(t *testing.T) {
	s := watchEnv(t)
	ctx := context.Background()
	sent := 0
	fail := false
	old := sendAccessCard
	sendAccessCard = func(context.Context, *config.Settings, *accessRequest, *config.ChatRule) (int64, error) {
		if fail {
			return 0, errors.New("bot api down")
		}
		sent++
		return 777, nil
	}
	t.Cleanup(func() { sendAccessCard = old })
	put := func(r *accessRequest) {
		if err := editAccess(s, func(m map[string]*accessRequest) bool { m[r.ID] = r; return true }); err != nil {
			t.Fatal(err)
		}
	}
	put(&accessRequest{ID: "live", Peer: 1, Status: accessPending, PubPID: os.Getpid(), PubSince: pubNow(), Created: time.Now()})
	put(&accessRequest{ID: "dead", Peer: 2, Status: accessPending, PubPID: 999999, PubSince: pubNow(), Created: time.Now()})
	put(&accessRequest{ID: "done", Peer: 3, Status: accessPending, CardID: 5, Created: time.Now()})

	if ok, err := adoptOrphan(ctx, s, "live"); ok || err != nil {
		t.Fatalf("взят запрос, который отправляет живой процесс: %v %v", ok, err)
	}
	fail = true
	if ok, err := adoptOrphan(ctx, s, "dead"); !ok || err == nil {
		t.Fatalf("сбой карточки: %v %v", ok, err)
	}
	if r := loadAccess(s)["dead"]; r == nil || r.CardID != 0 || r.PubPID != 0 {
		t.Fatalf("после сбоя запрос не вернулся в сироты: %+v", r)
	}
	fail = false
	publishOrphanAccess(ctx, s)
	publishOrphanAccess(ctx, s)
	all := loadAccess(s)
	if sent != 1 || all["dead"].CardID != 777 || all["dead"].PubPID != 0 || all["live"].CardID != 0 || all["done"].CardID != 5 {
		t.Fatalf("карточек %d, запросы: dead %+v live %+v done %+v", sent, all["dead"], all["live"], all["done"])
	}
}

// Одобренное, которое никто не отправил (одобривший процесс умер), служба
// досылает из уборки; пока черновик держит живой захват — нет.
func TestRecoverOrphanApproved(t *testing.T) {
	s := watchEnv(t)
	s.SendPolicy = "bot_approval"
	ctx := context.Background()
	box := outbox.New(s.DataDir())
	d, err := box.Create(outbox.CreateOpts{Chat: "closed-chat", Text: "раз", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if d, err = box.Approve(d.ID, "auto_send"); err != nil {
		t.Fatal(err)
	}
	c, _, err := box.ClaimSend(d.ID)
	if c == nil || err != nil {
		t.Fatalf("захват: %v", err)
	}
	later := time.Now().Add(time.Hour)
	recoverApproved(ctx, s, later, "test")
	if got, _ := box.Get(d.ID); got.SendError != nil || got.Status != outbox.Approved {
		t.Fatalf("досылка взяла черновик с живым захватом: %+v", got)
	}
	c.Release()
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond) // без 15-секундных повторов
	defer cancel()
	recoverApproved(short, s, later, "test")
	got, _ := box.Get(d.ID)
	if got.SendError == nil { // чат закрыт — отправка честно сорвалась, но её попробовали
		t.Fatalf("недосланное не досылалось: %+v", got)
	}
	if raw, _ := os.ReadFile(s.AuditPath()); !strings.Contains(string(raw), "recover_interrupted") {
		t.Fatal("в аудите нет досылки")
	}
	// уже сорвавшееся не долбим повторно
	recoverApproved(ctx, s, later, "test")
	if raw, _ := os.ReadFile(s.AuditPath()); strings.Count(string(raw), "recover_interrupted") != 2 {
		t.Fatalf("повторная досылка сорвавшегося: %s", raw)
	}
}
