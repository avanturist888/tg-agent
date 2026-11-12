package core

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/omap"
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

// Досылку оборвала остановка службы — это не сбой отправки: черновик
// остаётся одобренным без ошибки, его дошлёт следующий проход.
func TestRecoverCancelledNotFailed(t *testing.T) {
	s := watchEnv(t)
	s.SendPolicy = "bot_approval"
	box := outbox.New(s.DataDir())
	d, err := box.Create(outbox.CreateOpts{Chat: "closed-chat", Text: "раз", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = box.Approve(d.ID, "auto_send"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := deliver(ctx, s, d.ID, "test"); got != "" {
		t.Fatalf("итог при обрыве: %q", got)
	}
	if got, _ := box.Get(d.ID); got.SendError != nil || got.Status != outbox.Approved {
		t.Fatalf("обрыв записан ошибкой: %+v", got)
	}
	if raw, _ := os.ReadFile(s.AuditPath()); !strings.Contains(string(raw), "send_interrupted") {
		t.Fatal("в аудите нет обрыва")
	}
	// остановка службы — проход не берёт следующие черновики
	recoverApproved(ctx, s, time.Now().Add(time.Hour), "test")
	if raw, _ := os.ReadFile(s.AuditPath()); strings.Contains(string(raw), "recover_interrupted") {
		t.Fatal("досылка пошла после остановки")
	}
}

// Часть сообщения ушла, и тут служба останавливается: отказ Denied
// («часть ушла») главнее обрыва — черновик помечен ошибкой, досылка его не
// повторит (иначе ушедшая часть задвоилась бы).
func TestRecoverCancelledAfterPartial(t *testing.T) {
	s := watchEnv(t)
	box := outbox.New(s.DataDir())
	d := approved(t, box, "test")
	ctx, cancel := context.WithCancel(context.Background())
	old := sendClaimed
	sendClaimed = func(context.Context, *config.Settings, string, string) (*omap.Map, error) {
		cancel() // остановка службы посреди отправки
		return nil, &Denied{Msg: "часть сообщения ушла (1 из 2 файлов)"}
	}
	t.Cleanup(func() { sendClaimed = old })
	if got := deliver(ctx, s, d.ID, "test"); !strings.Contains(got, "Не отправлено") {
		t.Fatalf("итог: %q", got)
	}
	if got, _ := box.Get(d.ID); got.SendError == nil {
		t.Fatalf("частичная отправка не записана: %+v", got)
	}
}

// Пока досылка шла по своему списку, другой путь отправил часть черновика
// и записал отказ: досылка смотрит на черновик в момент захвата и его не
// трогает. Частичная отправка при досылке — тоже без второго прохода.
func TestRecoverSkipsFreshFailure(t *testing.T) {
	s := watchEnv(t)
	s.SendPolicy = "bot_approval"
	ctx := context.Background()
	box := outbox.New(s.DataDir())
	mk := func() *outbox.Draft {
		d, err := box.Create(outbox.CreateOpts{Chat: "test", Text: "раз", TTLMin: 60})
		if err != nil {
			t.Fatal(err)
		}
		if d, err = box.Approve(d.ID, "auto_send"); err != nil {
			t.Fatal(err)
		}
		return d
	}
	first, second := mk(), mk()
	sends := map[string]int{}
	old := sendClaimed
	sendClaimed = func(_ context.Context, _ *config.Settings, id, _ string) (*omap.Map, error) {
		sends[id]++
		if id == first.ID {
			// другой путь тем временем: часть второго ушла, отказ записан
			if _, err := box.MarkSendError(second.ID, "часть сообщения ушла"); err != nil {
				t.Fatal(err)
			}
			return nil, &Denied{Msg: "часть сообщения ушла"}
		}
		return omap.New().Set("message_id", 1), nil
	}
	t.Cleanup(func() { sendClaimed = old })
	later := time.Now().Add(time.Hour)
	recoverApproved(ctx, s, later, "test")
	recoverApproved(ctx, s, later, "test")
	if sends[first.ID] != 1 || sends[second.ID] != 0 {
		t.Fatalf("отправки: %v", sends)
	}
}

// Автоотправка взяла пачку одобренных; пока она шла, досылка частично
// отправила один и записала отказ. Дойдя до него, автоотправка (как и
// кнопка) его не трогает: сбой проверяется при захвате у любого пути.
func TestDeliverSkipsRecordedFailure(t *testing.T) {
	s := watchEnv(t)
	box := outbox.New(s.DataDir())
	d := approved(t, box, "test")
	if _, err := box.MarkSendError(d.ID, "часть сообщения ушла"); err != nil {
		t.Fatal(err)
	}
	called := 0
	old := sendClaimed
	sendClaimed = func(context.Context, *config.Settings, string, string) (*omap.Map, error) {
		called++
		return omap.New().Set("message_id", 1), nil
	}
	t.Cleanup(func() { sendClaimed = old })
	if got := deliver(context.Background(), s, d.ID, "auto_send"); got != "" || called != 0 {
		t.Fatalf("отправлен черновик с записанным сбоем: %q, вызовов %d", got, called)
	}
}

// Сообщение ушло, а отметка «отправлено» не записалась: второй раз его не
// отправить (и из другого процесса — отметка в файле), а уборка службы
// дописывает её, как только база примет.
func TestMarkSentDurably(t *testing.T) {
	s := watchEnv(t)
	box := outbox.New(s.DataDir())
	d := approved(t, box, "test")
	db, err := stateDB(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Q().Exec(`CREATE TRIGGER no_sent BEFORE UPDATE ON drafts WHEN NEW.status = 'sent'
		BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	was := markSentRetry
	markSentRetry.quick = time.Millisecond
	t.Cleanup(func() { markSentRetry = was })
	markSentDurably(s, box, d.ID, 77)
	if mid, ok := unsavedSent(s, d.ID); !ok || mid != 77 {
		t.Fatal("ушедшее без отметки не запомнено")
	}
	var dn *Denied
	if _, err := deliverClaimed(context.Background(), s, d.ID, "test"); !errors.As(err, &dn) {
		t.Fatalf("повторная отправка не отказана: %v", err)
	}
	finishUnsaved(s) // база всё ещё не принимает — файл остаётся
	if _, ok := unsavedSent(s, d.ID); !ok {
		t.Fatal("отметка потеряна, пока база не принимает")
	}
	if _, err := db.Q().Exec(`DROP TRIGGER no_sent`); err != nil {
		t.Fatal(err)
	}
	finishUnsaved(s)
	got, _ := box.Get(d.ID)
	if _, ok := unsavedSent(s, d.ID); ok || got.Status != outbox.Sent || got.MessageID == nil || *got.MessageID != 77 {
		t.Fatalf("отметка не дописана: %+v", got)
	}
}
