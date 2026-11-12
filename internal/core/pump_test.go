package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"tgagent/internal/bot"
	"tgagent/internal/config"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
)

// Решение владельца (кнопка черновика) сдвигает записанное смещение только
// после разбора: умри процесс посреди, Telegram отдаст нажатие снова.
// Остальное (переключатель автоотправки) — до разбора: повтор переключил бы
// обратно.
func TestPumpOffsetOrder(t *testing.T) {
	s := watchEnv(t)
	s.BotToken = "77:x"
	seen := map[int64]int64{} // update_id → записанное смещение во время разбора
	old := updateHandler
	updateHandler = func(_ context.Context, s *config.Settings, u bot.Update) error {
		seen[u.UpdateID] = readOffset(s)
		return nil
	}
	t.Cleanup(func() { updateHandler = old })
	cb := func(id int64, data string) bot.Update {
		return bot.Update{UpdateID: id, CallbackQuery: &bot.CallbackQuery{ID: "q", Data: data}}
	}
	off, _ := pumpUpdates(context.Background(), s, 10, []bot.Update{
		cb(10, "d:abc:ok"), cb(11, "s:chat~y"), cb(12, "@work|d:x:no"),
	})
	if off != 13 || readOffset(s) != 13 {
		t.Fatalf("смещение после пачки: %d, записано %d", off, readOffset(s))
	}
	if seen[10] != 10 || seen[12] != 12 {
		t.Fatalf("решение сдвинуло смещение до разбора: %v", seen)
	}
	if seen[11] != 12 {
		t.Fatalf("переключатель — смещение не сдвинуто до разбора: %v", seen)
	}
}

// Решение, разбор которого раз за разом роняет процесс, пропускается после
// maxUpdateTries начал — служба не падает по кругу.
func TestPumpPoisonSkipped(t *testing.T) {
	s := watchEnv(t)
	s.BotToken = "77:x"
	handled := 0
	old := updateHandler
	updateHandler = func(context.Context, *config.Settings, bot.Update) error { handled++; return nil }
	t.Cleanup(func() { updateHandler = old })
	u := bot.Update{UpdateID: 5, CallbackQuery: &bot.CallbackQuery{ID: "q", Data: "d:abc:ok"}}
	// процесс «умирал» посреди разбора: начат, смещение не сдвинуто
	for range maxUpdateTries {
		startInflight(s, 5, 5)
	}
	if off, _ := pumpUpdates(context.Background(), s, readOffset(s), []bot.Update{u}); off != 6 || handled != 0 {
		t.Fatalf("смещение %d, разобрано %d", off, handled)
	}
	if readOffset(s) != 6 {
		t.Fatalf("пропущенное не сдвинуло смещение: %d", readOffset(s))
	}
}

// Решение не принялось из-за сбоя (паника, служба аккаунта не ответила) —
// смещение за ним не сдвигается, пачка обрывается: нажатие разберётся снова.
func TestPumpRetryNotAcked(t *testing.T) {
	s := watchEnv(t)
	s.BotToken = "77:x"
	calls := 0
	old := updateHandler
	updateHandler = func(context.Context, *config.Settings, bot.Update) error {
		calls++
		if calls == 1 {
			return retryable{errors.New("служба work не ответила")}
		}
		return nil
	}
	t.Cleanup(func() { updateHandler = old })
	batch := []bot.Update{
		{UpdateID: 20, CallbackQuery: &bot.CallbackQuery{ID: "q", Data: "@work|d:x:ok"}},
		{UpdateID: 21, CallbackQuery: &bot.CallbackQuery{ID: "q2", Data: "d:y:ok"}},
	}
	off, stalled := pumpUpdates(context.Background(), s, 20, batch)
	if !stalled || off != 20 || readOffset(s) != 20 || calls != 1 {
		t.Fatalf("сбой: смещение %d (записано %d), stalled %v, разборов %d", off, readOffset(s), stalled, calls)
	}
	off, stalled = pumpUpdates(context.Background(), s, off, batch)
	if stalled || off != 22 || readOffset(s) != 22 || calls != 3 {
		t.Fatalf("повтор: смещение %d, stalled %v, разборов %d", off, stalled, calls)
	}
}

// Паника в разборе нажатия — сбой (повторить), а не принятое решение.
func TestHandleCallbackPanicRetry(t *testing.T) {
	s := watchEnv(t)
	s.ApprovalChatID = 1
	d, err := outbox.New(s.DataDir()).Create(outbox.CreateOpts{Chat: "test", Text: "x", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	q := &bot.CallbackQuery{ID: "q", Data: "d:" + d.ID + ":ok"}
	q.From.ID = 1
	old := sendClaimed
	sendClaimed = func(context.Context, *config.Settings, string, string) (*omap.Map, error) { panic("сбой") }
	t.Cleanup(func() { sendClaimed = old })
	if err := HandleCallback(context.Background(), s, q); !isRetry(err) {
		t.Fatalf("паника — не сбой: %v", err)
	}
	// повтор того же нажатия видит принятое решение и ничего не делает
	if err := HandleCallback(context.Background(), s, q); err != nil {
		t.Fatalf("повтор принятого решения: %v", err)
	}
}

// Решение по запросу доступа не записалось (сбой базы) — нажатие не
// принято: retryable, запрос ждёт, повтор его решает.
func TestAccessDecisionRetry(t *testing.T) {
	s := watchEnv(t)
	s.ApprovalChatID = 1
	if err := editAccess(s, func(m map[string]*accessRequest) bool {
		m["r1"] = &accessRequest{ID: "r1", Peer: 5, Status: accessPending, Created: time.Now()}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	q := &bot.CallbackQuery{ID: "q", Data: "a:r1:no"}
	q.From.ID = 1
	old := saveAccessDecision
	saveAccessDecision = func(*config.Settings, func(map[string]*accessRequest) bool) error {
		return errors.New("database is locked")
	}
	if err := HandleCallback(context.Background(), s, q); !isRetry(err) {
		t.Fatalf("сбой записи — не повтор: %v", err)
	}
	saveAccessDecision = old
	if r := loadAccess(s)["r1"]; r == nil || r.Status != accessPending {
		t.Fatalf("после сбоя запрос не ждёт: %+v", r)
	}
	if err := HandleCallback(context.Background(), s, q); err != nil {
		t.Fatal(err)
	}
	if r := loadAccess(s)["r1"]; r == nil || r.Status != accessDenied {
		t.Fatalf("повтор не решил запрос: %+v", r)
	}
}
