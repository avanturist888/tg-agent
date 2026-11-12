package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tgagent/internal/bot"
	"tgagent/internal/config"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/store"
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
	said := []string{}
	sayFails := true
	oldSay := sayErr
	sayErr = func(_ context.Context, _ *config.Settings, text string) error {
		if sayFails {
			return errors.New("bot api down")
		}
		said = append(said, text)
		return nil
	}
	t.Cleanup(func() { sayErr = oldSay })
	// просьба владельцу не дошла — нажатие не пропускается
	if off, stalled := pumpUpdates(context.Background(), s, readOffset(s), []bot.Update{u}); off != 5 || !stalled || handled != 0 || readOffset(s) != 5 {
		t.Fatalf("просьба не дошла, а смещение %d (записано %d), stalled %v", off, readOffset(s), stalled)
	}
	sayFails = false
	if off, _ := pumpUpdates(context.Background(), s, readOffset(s), []bot.Update{u}); off != 6 || handled != 0 || len(said) != 1 {
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

// Решение записано, а итог не доведён (процесс умер посреди tellAccess,
// нажатие уже подтверждено) — уборка доводит его и отмечает сказанным;
// свежее не трогает.
func TestAccessRetold(t *testing.T) {
	s := watchEnv(t)
	put := func(r *accessRequest) {
		if err := editAccess(s, func(m map[string]*accessRequest) bool { m[r.ID] = r; return true }); err != nil {
			t.Fatal(err)
		}
	}
	put(&accessRequest{ID: "old", Peer: 5, Status: accessDenied, Result: "✋ **Отказано**", Decided: time.Now().Add(-time.Hour)})
	put(&accessRequest{ID: "fresh", Peer: 6, Status: accessDenied, Result: "✋ **Отказано**", Decided: time.Now()})
	put(&accessRequest{ID: "legacy", Peer: 7, Status: accessDenied, Decided: time.Now().Add(-time.Hour)})
	retellAccess(context.Background(), s)
	all := loadAccess(s)
	if !all["old"].Told || all["fresh"].Told || all["legacy"].Told {
		t.Fatalf("доведено: old %v, fresh %v, legacy %v", all["old"].Told, all["fresh"].Told, all["legacy"].Told)
	}
}

// Пока перенос старых файлов ждёт (нет отметки tg cutover), решение
// владельца не разбирается — сбой, а не «черновик не найден».
func TestDecisionWaitsLegacy(t *testing.T) {
	s := watchEnv(t)
	s.ApprovalChatID = 1
	store.RequireCutover = true
	t.Cleanup(func() { store.RequireCutover = false })
	if err := os.MkdirAll(s.DataDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.DataDir(), "outbox.json"), []byte(`[{"id": "old", "status": "pending"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	q := &bot.CallbackQuery{ID: "q", Data: "d:old:ok"}
	q.From.ID = 1
	if err := HandleCallback(context.Background(), s, q); !isRetry(err) {
		t.Fatalf("решение при ждущем переносе: %v", err)
	}
}

// Итог не показался в карточке (Bot API не ответил) — Told не ставится, и
// повтор доводит только карточку: агенту второй раз не пишет.
func TestAccessTellPartial(t *testing.T) {
	s := watchEnv(t)
	r := &accessRequest{ID: "p", Peer: 5, Status: accessDenied, Result: "✋", CardID: 9, Decided: time.Now().Add(-time.Hour)}
	if err := editAccess(s, func(m map[string]*accessRequest) bool { m[r.ID] = r; return true }); err != nil {
		t.Fatal(err)
	}
	cardOK, told := false, 0
	oldCard, oldAgent := finishAccessCard, tellAgentAccess
	finishAccessCard = func(context.Context, *config.Settings, *accessRequest, string) bool { return cardOK }
	tellAgentAccess = func(context.Context, *accessRequest) bool { told++; return true }
	t.Cleanup(func() { finishAccessCard, tellAgentAccess = oldCard, oldAgent })
	retellAccess(context.Background(), s)
	if got := loadAccess(s)["p"]; got.Told || got.CardShown || !got.AgentTold || told != 1 {
		t.Fatalf("после сбоя карточки: %+v, агенту %d", got, told)
	}
	cardOK = true
	retellAccess(context.Background(), s)
	if got := loadAccess(s)["p"]; !got.Told || told != 1 {
		t.Fatalf("после повтора: %+v, агенту %d", got, told)
	}
}

// Карточка запроса не ушла (или ответ Bot API потерялся) — запрос не
// удаляется, а становится сиротой: карточку пришлёт уборка.
func TestPublishAccessFailureKeepsRequest(t *testing.T) {
	s := watchEnv(t)
	old := sendAccessCard
	sendAccessCard = func(context.Context, *config.Settings, *accessRequest, *config.ChatRule) (int64, error) {
		return 0, errors.New("timeout")
	}
	t.Cleanup(func() { sendAccessCard = old })
	req := &accessRequest{ID: "x", Peer: 5, Status: accessPending, Created: time.Now()}
	sent, err := publishAccess(context.Background(), s, req, nil)
	if err != nil || sent {
		t.Fatalf("сбой карточки: sent %v, err %v", sent, err)
	}
	if r := loadAccess(s)[req.ID]; r == nil || !r.orphaned() {
		t.Fatalf("запрос после сбоя карточки: %+v", r)
	}
}

// Владелец решил раньше, чем номер карточки записался: итог в карточку не
// считается показанным, его доведёт уборка.
func TestAccessCardRecordedAfterDecision(t *testing.T) {
	s := watchEnv(t)
	r := &accessRequest{ID: "y", Peer: 6, Status: accessDenied, Result: "✋", CardShown: true, AgentTold: true, Told: true, Decided: time.Now()}
	if err := editAccess(s, func(m map[string]*accessRequest) bool { m[r.ID] = r; return true }); err != nil {
		t.Fatal(err)
	}
	if err := recordAccessCard(s, &accessRequest{ID: "y"}, 77); err != nil {
		t.Fatal(err)
	}
	if got := loadAccess(s)["y"]; got.CardShown || got.Told || !got.AgentTold || got.CardID != 77 {
		t.Fatalf("после привязки карточки: %+v", got)
	}
}

// Номер карточки записался, пока разбирали нажатие, — решение его не
// затирает (пишется в свежую строку).
func TestAccessDecisionKeepsCardID(t *testing.T) {
	s := watchEnv(t)
	s.ApprovalChatID = 1
	if err := editAccess(s, func(m map[string]*accessRequest) bool {
		m["z"] = &accessRequest{ID: "z", Peer: 5, Status: accessPending, PubPID: 999999, Created: time.Now()}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	old := saveAccessDecision
	saveAccessDecision = func(s *config.Settings, fn func(map[string]*accessRequest) bool) error {
		if err := recordAccessCard(s, &accessRequest{ID: "z"}, 77); err != nil { // публикация — между чтением и записью
			return err
		}
		return editAccess(s, fn)
	}
	t.Cleanup(func() { saveAccessDecision = old })
	q := &bot.CallbackQuery{ID: "q", Data: "a:z:no"}
	q.From.ID = 1
	if err := HandleCallback(context.Background(), s, q); err != nil {
		t.Fatal(err)
	}
	if got := loadAccess(s)["z"]; got.Status != accessDenied || got.CardID != 77 {
		t.Fatalf("после решения: %+v", got)
	}
}

// Номер карточки неизвестен (ответ Bot API потерялся) — итог владельцу
// отдельным сообщением, а не «показан» молча.
func TestAccessCardLostSaid(t *testing.T) {
	s := watchEnv(t)
	var said []string
	old := sayErr
	sayErr = func(_ context.Context, _ *config.Settings, text string) error { said = append(said, text); return nil }
	t.Cleanup(func() { sayErr = old })
	r := &accessRequest{ID: "l", Title: "Чат", Status: accessDenied, CardLost: true, Result: "✋ **Отказано** — белый список не менялся."}
	if !finishAccessCard(context.Background(), s, r, r.Result) || len(said) != 1 || strings.Contains(said[0], "**") {
		t.Fatalf("сказано: %q", said)
	}
}
