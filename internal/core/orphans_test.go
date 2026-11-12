package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/lock"
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

// Досылку оборвала остановка службы посреди попытки: ушло ли — неизвестно,
// поэтому это сбой, а не «дошлём потом»: досылка его больше не трогает.
func TestRecoverCancelledIsFailure(t *testing.T) {
	s := watchEnv(t)
	s.SendPolicy = "bot_approval"
	box := outbox.New(s.DataDir())
	d, err := box.Create(outbox.CreateOpts{Chat: "test", Text: "раз", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Approve(d.ID, "auto_send"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	old := sendClaimed
	sendClaimed = func(context.Context, *config.Settings, string, string) (*omap.Map, error) {
		calls++
		cancel() // служба останавливается посреди отправки
		return nil, context.Canceled
	}
	t.Cleanup(func() { sendClaimed = old })
	if got := deliver(ctx, s, d.ID, "auto_send"); !strings.Contains(got, "Не отправлено") || calls != 1 {
		t.Fatalf("итог при обрыве: %q, отправок %d", got, calls)
	}
	if got, _ := box.Get(d.ID); got.SendError == nil {
		t.Fatalf("обрыв не записан: %+v", got)
	}
	recoverApproved(context.Background(), s, time.Now().Add(time.Hour), "test")
	if calls != 1 {
		t.Fatalf("досылка повторила оборванное: %d", calls)
	}
}

// Ошибка после начала отправки без пометки — «могло уйти»: без повтора и
// с записанным сбоем; точно не дошедшее (NotSent) повторяется.
func TestDeliverRetriesOnlyNotSent(t *testing.T) {
	if _, ok := classifySend(errors.New("i/o timeout")).(*Uncertain); !ok {
		t.Fatal("сбой без пометки не стал Uncertain")
	}
	var ns *NotSent
	if !errors.As(classifySend(notSent(errors.New("нет связи"))), &ns) {
		t.Fatal("NotSent потерян")
	}
	s := watchEnv(t)
	box := outbox.New(s.DataDir())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	run := func(fail func(n int) error) (int, *outbox.Draft) {
		d := approved(t, box, "test")
		calls := 0
		old := sendClaimed
		sendClaimed = func(context.Context, *config.Settings, string, string) (*omap.Map, error) {
			calls++
			if err := fail(calls); err != nil {
				return nil, err
			}
			return omap.New().Set("message_id", 1), nil
		}
		defer func() { sendClaimed = old }()
		deliver(ctx, s, d.ID, "test")
		got, _ := box.Get(d.ID)
		return calls, got
	}
	if calls, got := run(func(int) error { return &Uncertain{Err: errors.New("i/o timeout")} }); calls != 1 || got.SendError == nil {
		t.Fatalf("могло уйти: отправок %d, черновик %+v", calls, got)
	}
	was := retryPause
	retryPause = time.Millisecond
	t.Cleanup(func() { retryPause = was })
	if calls, _ := run(func(n int) error {
		if n == 1 {
			return notSent(errors.New("нет связи"))
		}
		return nil
	}); calls != 2 {
		t.Fatalf("точно не дошедшее не повторено: %d", calls)
	}
}

// Попытка была (захват записан), а итога нет — процесс умер посреди
// отправки или не смог записать ни отметку, ни ошибку. Сам такой черновик
// больше не отправляется: сбой «могло уйти» — решает человек или агент.
func TestDeliverAfterUnfinishedAttempt(t *testing.T) {
	s := watchEnv(t)
	box := outbox.New(s.DataDir())
	d := approved(t, box, "test")
	c, _, err := box.ClaimSend(d.ID)
	if err != nil || c == nil {
		t.Fatalf("захват: %v", err)
	}
	if c.Retry {
		t.Fatal("первая попытка помечена повтором")
	}
	c.Release() // итога нет
	called := 0
	old := sendClaimed
	sendClaimed = func(context.Context, *config.Settings, string, string) (*omap.Map, error) {
		called++
		return omap.New().Set("message_id", 1), nil
	}
	t.Cleanup(func() { sendClaimed = old })
	if got := deliver(context.Background(), s, d.ID, "auto_send"); !strings.Contains(got, "могло уйти") || called != 0 {
		t.Fatalf("итог %q, отправок %d", got, called)
	}
	if got, _ := box.Get(d.ID); got.SendError == nil || got.Status != outbox.Approved {
		t.Fatalf("черновик: %+v", got)
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
	rule := s.Chats["test"]
	rule.Send = true // иначе deliverClaimed откажет раньше, чем дойдёт до отметки
	s.Chats["test"] = rule
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
	if _, err := deliverClaimed(context.Background(), s, d.ID, "test"); !errors.As(err, &dn) || !strings.Contains(err.Error(), "отметка в базу") {
		t.Fatalf("повторная отправка не отказана отметкой: %v", err)
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

// Итог не показался в карточке (Bot API не отвечал) — он запомнен, и уборка
// показывает его, когда Bot API ответит, без перезапуска службы.
func TestCardRetried(t *testing.T) {
	s := watchEnv(t)
	box := outbox.New(s.DataDir())
	d := approved(t, box, "test")
	cardID := int64(55)
	up := false
	var shown []string
	old := finishCard
	finishCard = func(_ context.Context, _ *config.Settings, id int64, _ *outbox.Draft, _, status, _ string) bool {
		if up {
			shown = append(shown, status)
		}
		return up
	}
	t.Cleanup(func() { finishCard = old })
	card(context.Background(), s, &cardID, d, "⚠️ **Не отправлено** — сбой")
	retryCards(context.Background(), s) // Bot API всё ещё молчит
	up = true
	retryCards(context.Background(), s)
	retryCards(context.Background(), s)
	if len(shown) != 1 || !strings.Contains(shown[0], "Не отправлено") {
		t.Fatalf("показано: %q", shown)
	}
}

// Уборка показывает прежний итог, а тем временем (другой процесс) пришёл
// новый и тоже не показался: уборка не отмечает показанным новый — он
// остаётся в очереди. Промежуточное «Отправляю…» в очередь не идёт.
func TestCardRetryKeepsNewer(t *testing.T) {
	s := watchEnv(t)
	box := outbox.New(s.DataDir())
	d := approved(t, box, "test")
	db, err := stateDB(s)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cardID := int64(55)
	up := false
	old := finishCard
	finishCard = func(_ context.Context, _ *config.Settings, _ int64, _ *outbox.Draft, _, status, _ string) bool {
		if up && strings.Contains(status, "первый") {
			// пока уборка показывает первый, другой процесс принял новый итог
			v, err := nextCardVersion(ctx, db, d.ID)
			if err != nil {
				t.Fatal(err)
			}
			rememberCard(ctx, s, db, d.ID, cardID, "⚠️ второй", v, false)
		}
		return up
	}
	t.Cleanup(func() { finishCard = old })
	card(ctx, s, &cardID, d, "⚠️ первый")
	card(ctx, s, &cardID, d, "⏳ **Отправляю…**") // не показалось, но и не заменило первый
	if st, _, _ := cardStates.Get(ctx, db.Q(), d.ID); !st.pending() || st.Status != "⚠️ первый" {
		t.Fatalf("в очереди: %+v", st)
	}
	up = true
	retryCards(ctx, s)
	if st, _, _ := cardStates.Get(ctx, db.Q(), d.ID); !st.pending() || st.Status != "⚠️ второй" {
		t.Fatalf("новый итог потерян: %+v", st)
	}
	retryCards(ctx, s)
	if st, _, _ := cardStates.Get(ctx, db.Q(), d.ID); st.pending() {
		t.Fatalf("показанный итог остался в очереди: %+v", st)
	}
}

// Другой процесс держит лок карточки: показывает новый итог. Уборка этого
// процесса ждёт лок, перечитывает состояние и прежний итог поверх нового не
// показывает.
func TestCardRetryWaitsOtherProcess(t *testing.T) {
	s := watchEnv(t)
	box := outbox.New(s.DataDir())
	d := approved(t, box, "test")
	db, err := stateDB(s)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	v, err := nextCardVersion(ctx, db, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	rememberCard(ctx, s, db, d.ID, 55, "⚠️ старый", v, false)
	var shown []string
	old := finishCard
	finishCard = func(_ context.Context, _ *config.Settings, _ int64, _ *outbox.Draft, _, status, _ string) bool {
		shown = append(shown, status)
		return true
	}
	t.Cleanup(func() { finishCard = old })
	other := lock.New(filepath.Join(s.DataDir(), "cards", d.ID), time.Second)
	os.MkdirAll(filepath.Join(s.DataDir(), "cards"), 0o700)
	if err := other.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { retryCards(ctx, s); close(done) }()
	time.Sleep(200 * time.Millisecond)
	// «другой процесс» показал новый итог
	v2, err := nextCardVersion(ctx, db, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	rememberCard(ctx, s, db, d.ID, 55, "✅ новый", v2, true)
	other.Release()
	<-done
	if len(shown) != 0 {
		t.Fatalf("прежний итог показан поверх нового: %q", shown)
	}
}

// Итог, появившийся раньше, но опоздавший (не дождался лока или взял его
// после более нового), не показывается и в очередь не встаёт: новый уже
// показан и из очереди ушёл, но его номер остался.
func TestCardDelayedOlderRejected(t *testing.T) {
	s := watchEnv(t)
	box := outbox.New(s.DataDir())
	d := approved(t, box, "test")
	db, err := stateDB(s)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var shown []string
	old := finishCard
	finishCard = func(_ context.Context, _ *config.Settings, _ int64, _ *outbox.Draft, _, status, _ string) bool {
		shown = append(shown, status)
		return true
	}
	t.Cleanup(func() { finishCard = old })
	cardID := int64(55)

	// A получил номер, не дождался лока; тем временем B показал свой итог
	vA, err := nextCardVersion(ctx, db, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	card(ctx, s, &cardID, d, "✅ B")
	rememberCard(ctx, s, db, d.ID, cardID, "⚠️ A", vA, false)
	if st, _, _ := cardStates.Get(ctx, db.Q(), d.ID); st.pending() || st.Status != "✅ B" {
		t.Fatalf("старый итог принят поверх нового: %+v", st)
	}
	retryCards(ctx, s)
	if len(shown) != 1 || shown[0] != "✅ B" {
		t.Fatalf("показано: %q", shown)
	}

	// A получил номер и ждёт лок; B (другой процесс) показал итог, пока A
	// ждал; A взял лок после B — показывать нечего
	shown = nil
	other := lock.New(filepath.Join(s.DataDir(), "cards", d.ID), time.Second)
	if err := other.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { card(ctx, s, &cardID, d, "⚠️ A2"); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, _, _ := cardStates.Get(ctx, db.Q(), d.ID)
		if st.Next > vA+1 { // A2 уже получил номер
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("A2 не получил номер")
		}
		time.Sleep(10 * time.Millisecond)
	}
	vB, err := nextCardVersion(ctx, db, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	rememberCard(ctx, s, db, d.ID, cardID, "✅ B2", vB, true)
	other.Release()
	<-done
	if len(shown) != 0 {
		t.Fatalf("опоздавший итог показан поверх нового: %q", shown)
	}
	if st, _, _ := cardStates.Get(ctx, db.Q(), d.ID); st.pending() || st.Status != "✅ B2" {
		t.Fatalf("состояние: %+v", st)
	}
}

// Удалённый черновик — и состояние его карточки уборка забывает.
func TestCardStateForgottenWithDraft(t *testing.T) {
	s := watchEnv(t)
	db, err := stateDB(s)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	v, err := nextCardVersion(ctx, db, "nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	rememberCard(ctx, s, db, "nonexistent", 55, "⚠️ итог", v, false)
	retryCards(ctx, s)
	if _, ok, _ := cardStates.Get(ctx, db.Q(), "nonexistent"); ok {
		t.Fatal("состояние карточки удалённого черновика осталось")
	}
}
