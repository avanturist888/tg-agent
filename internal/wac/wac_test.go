package wac

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestFromMarkdown(t *testing.T) {
	cases := map[string]string{
		"**жирный** и *курсив*":          "*жирный* и _курсив_",
		"__тоже жирный__, ~~нет~~":       "*тоже жирный*, ~нет~",
		"[сайт](https://e.com) и `a*b*`": "сайт (https://e.com) и `a*b*`",
		"## Заголовок\nтекст":            "*Заголовок*\nтекст",
		"```\n**код**\n```":              "```\n**код**\n```",
		"2*3*4 = 24":                     "2*3*4 = 24",
	}
	for in, want := range cases {
		if got := FromMarkdown(in); got != want {
			t.Errorf("%q → %q, ждали %q", in, got, want)
		}
	}
}

func TestParse(t *testing.T) {
	m := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("ответ"),
		ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("ABC")}}}
	if c := Parse(m); c.Text != "ответ" || c.Kind != "" || c.Context.GetStanzaID() != "ABC" {
		t.Fatalf("%+v", c)
	}
	if c := Parse(&waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true)}}); c.Kind != "voice" {
		t.Fatalf("голосовое: %+v", c)
	}
	if !Parse(&waE2E.Message{}).Empty() {
		t.Fatal("пустое не пустое")
	}
}

func TestStore(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	chat, err := st.ChatID(ctx, "79990001122@s.whatsapp.net", "user")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := st.ChatID(ctx, "79990001122@s.whatsapp.net", "user"); again != chat {
		t.Fatal("второй номер у того же чата")
	}
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	put := func(wa string, at time.Time, text string, live bool) int64 {
		id, fresh, err := st.Put(ctx, &Message{Chat: chat, WAID: wa, Sender: "79990001122@s.whatsapp.net", Time: at, Text: text, Live: live})
		if err != nil || !fresh {
			t.Fatalf("put %s: %v %v", wa, fresh, err)
		}
		return id
	}
	a := put("A", t0.Add(2*time.Minute), "второе", true)
	b := put("B", t0.Add(3*time.Minute), "третье про кота", true)
	old := put("OLD", t0, "первое (из истории)", false) // номер больше, время раньше
	if _, fresh, _ := st.Put(ctx, &Message{Chat: chat, WAID: "A", Time: t0, Live: true}); fresh {
		t.Fatal("повтор записан как новое")
	}
	ch, _ := st.Chat(ctx, chat)
	if ch.Unread != 2 || ch.LastTS != t0.Add(3*time.Minute).UnixMilli() {
		t.Fatalf("чат: %+v", ch)
	}
	all, err := st.History(ctx, chat, Query{Limit: 10})
	if err != nil || len(all) != 3 || all[0].ID != old || all[2].ID != b {
		t.Fatalf("порядок по времени: %+v %v", all, err)
	}
	// «после» — по приходу и только живые: история с большим номером и
	// старым временем в «новое» не попадает, а задержанное живое — попадает
	late := put("LATE", t0.Add(time.Minute), "задержанное живое", true)
	after, _ := st.History(ctx, chat, Query{After: a, Limit: 10})
	if len(after) != 2 || after[0].ID != b || after[1].ID != late {
		t.Fatalf("после второго: %+v", after)
	}
	if after, _ := st.History(ctx, chat, Query{After: b, Limit: 10}); len(after) != 1 || after[0].ID != late {
		t.Fatalf("после третьего (история не в счёт): %+v", after)
	}
	before, _ := st.History(ctx, chat, Query{Before: b, Limit: 10})
	if len(before) != 3 || before[0].ID != old || before[2].ID != a {
		t.Fatalf("до третьего: %+v", before)
	}
	if found, _ := st.History(ctx, chat, Query{Search: "ТРЕТЬЕ ПРО"}); len(found) != 1 || found[0].ID != b {
		t.Fatalf("поиск: %+v", found)
	}
	if st.Edit(ctx, chat, "A", "второе, исправлено", nil, t0.Add(time.Hour)) != a {
		t.Fatal("правка не нашла сообщение")
	}
	st.React(ctx, chat, "A", "me@s.whatsapp.net", "👍", t0)
	m, _ := st.Get(ctx, a)
	if m.Text != "второе, исправлено" || m.Edited.IsZero() || m.Reactions["me@s.whatsapp.net"] != "👍" {
		t.Fatalf("после правки: %+v", m)
	}
	if st.Delete(ctx, chat, "B", t0) != b {
		t.Fatal("удаление не нашло сообщение")
	}
	if left, _ := st.History(ctx, chat, Query{Limit: 10}); len(left) != 3 {
		t.Fatalf("удалённое видно: %d", len(left))
	}
	if _, _, err := st.Put(ctx, &Message{Chat: chat, WAID: "MINE", FromMe: true, Time: t0.Add(4 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if ch, _ := st.Chat(ctx, chat); ch.Unread != 0 {
		t.Fatalf("своё сообщение не сбросило непрочитанные: %d", ch.Unread)
	}
}

// Личка, известная под LID, остаётся тем же номером, когда приходит по
// номеру телефона (и наоборот).
func TestStoreAliases(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lid, _ := st.ChatID(ctx, "111@lid", "user")
	pn, _ := st.ChatID(ctx, "7999@s.whatsapp.net", "user", "111@lid")
	if pn != lid {
		t.Fatalf("номер завёл новый чат: %d != %d", pn, lid)
	}
	if again, _ := st.ChatID(ctx, "7999@s.whatsapp.net", "user"); again != lid {
		t.Fatalf("по номеру без LID: %d", again)
	}
	if ch, err := st.ChatByJID(ctx, "7999@s.whatsapp.net"); err != nil || ch.ID != lid {
		t.Fatalf("поиск по псевдониму: %+v %v", ch, err)
	}
}

// Удаление, правка и реакция, пришедшие раньше самого сообщения, ждут его.
func TestStorePending(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	chat, _ := st.ChatID(ctx, "g@g.us", "group")
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	if st.Delete(ctx, chat, "GONE", t0.Add(time.Minute)) != 0 || st.Edit(ctx, chat, "FIX", "новое", nil, t0.Add(2*time.Minute)) != 0 {
		t.Fatal("нашлось несуществующее")
	}
	st.Edit(ctx, chat, "FIX", "старее", nil, t0.Add(time.Minute)) // правка старше — не должна победить
	st.React(ctx, chat, "FIX", "x@s.whatsapp.net", "🔥", t0.Add(time.Minute))
	gone, _, _ := st.Put(ctx, &Message{Chat: chat, WAID: "GONE", Time: t0, Text: "удалено у всех"})
	fix, _, _ := st.Put(ctx, &Message{Chat: chat, WAID: "FIX", Time: t0, Text: "было"})
	if m, _ := st.Get(ctx, gone); !m.Deleted || m.Text != "" {
		t.Fatalf("удаление не применилось: %+v", m)
	}
	if m, _ := st.Get(ctx, fix); m.Text != "новое" || m.Reactions["x@s.whatsapp.net"] != "🔥" {
		t.Fatalf("правка или реакция не применились: %+v", m)
	}
	if found, _ := st.History(ctx, chat, Query{Search: "НОВ"}); len(found) != 1 {
		t.Fatalf("поиск по правленому: %+v", found)
	}
}

// Личка успела завестись дважды — под LID и под номером, пока их связь не
// была известна; узнали — чаты сливаются в первый, номер второго ведёт туда.
func TestStoreMerge(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	lid, _ := st.ChatID(ctx, "111@lid", "user")
	pn, _ := st.ChatID(ctx, "7999@s.whatsapp.net", "user")
	if lid == pn {
		t.Fatal("без связи — уже один чат")
	}
	st.Put(ctx, &Message{Chat: lid, WAID: "L1", Time: t0, Text: "по LID", Live: true})
	st.Put(ctx, &Message{Chat: pn, WAID: "P1", Time: t0.Add(time.Minute), Text: "по номеру", Live: true})
	st.Put(ctx, &Message{Chat: pn, WAID: "L1", Time: t0, Text: "то же", Live: true}) // одно и то же в обоих
	got, err := st.ChatID(ctx, "7999@s.whatsapp.net", "user", "111@lid")
	if err != nil || got != lid {
		t.Fatalf("слили не в первый: %d %v", got, err)
	}
	if st.Resolve(ctx, pn) != lid {
		t.Fatal("номер слитого не ведёт на первый")
	}
	if ch, err := st.Chat(ctx, pn); err != nil || ch.ID != lid {
		t.Fatalf("чат по старому номеру: %+v %v", ch, err)
	}
	msgs, _ := st.History(ctx, pn, Query{Limit: 10})
	if len(msgs) != 2 || msgs[0].WAID != "L1" || msgs[1].WAID != "P1" {
		t.Fatalf("сообщения после слияния: %+v", msgs)
	}
	if again, _ := st.ChatID(ctx, "7999@s.whatsapp.net", "user"); again != lid {
		t.Fatalf("номер после слияния: %d", again)
	}
}

// Снятая реакция не возвращается, когда позже приходит история со старой.
func TestStoreReactionTombstone(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	chat, _ := st.ChatID(ctx, "g@g.us", "group")
	id, _, _ := st.Put(ctx, &Message{Chat: chat, WAID: "M", Time: t0, Text: "x"})
	st.React(ctx, chat, "M", "a", "👍", t0.Add(time.Minute))
	st.React(ctx, chat, "M", "a", "", t0.Add(2*time.Minute))
	st.React(ctx, chat, "M", "a", "👍", t0.Add(time.Minute)) // старая из истории
	if m, _ := st.Get(ctx, id); len(m.Reactions) != 0 {
		t.Fatalf("снятая реакция вернулась: %+v", m.Reactions)
	}
}

// Правки параллельно с появлением сообщения не теряются.
func TestStorePendingRace(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	chat, _ := st.ChatID(ctx, "g@g.us", "group")
	for i := 0; i < 30; i++ {
		wa := fmt.Sprintf("M%d", i)
		done := make(chan struct{})
		go func() {
			st.Edit(ctx, chat, wa, "правка", nil, t0.Add(time.Minute))
			close(done)
		}()
		id, _, _ := st.Put(ctx, &Message{Chat: chat, WAID: wa, Time: t0, Text: "было"})
		<-done
		if m, _ := st.Get(ctx, id); m.Text != "правка" {
			// правка пришла после Put и нашла сообщение — тоже правка
			t.Fatalf("%s: правка потерялась: %q", wa, m.Text)
		}
	}
}

// Слияние: номер слитого чата не достаётся новому; у одного сообщения в
// обоих чатах остаётся самое свежее состояние и все реакции; отложенное
// удаление, чьё сообщение было в другом чате, применяется; запись со старым
// номером попадает в рабочий чат.
func TestStoreMergeState(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	a, _ := st.ChatID(ctx, "111@lid", "user")
	b, _ := st.ChatID(ctx, "7999@s.whatsapp.net", "user")
	keep, _, _ := st.Put(ctx, &Message{Chat: a, WAID: "D", Time: t0, Text: "одно"})
	drop, _, _ := st.Put(ctx, &Message{Chat: b, WAID: "D", Time: t0, Text: "одно"})
	st.Delete(ctx, b, "D", t0.Add(time.Minute))          // удалено только во втором
	st.React(ctx, b, "D", "x", "🔥", t0.Add(time.Minute)) // реакция только во втором
	st.Put(ctx, &Message{Chat: b, WAID: "P", Time: t0, Text: "под номером"})
	st.Delete(ctx, a, "P", t0.Add(2*time.Minute)) // удаление пришло под LID — отложено
	if _, err := st.ChatID(ctx, "7999@s.whatsapp.net", "user", "111@lid"); err != nil {
		t.Fatal(err)
	}
	m, err := st.Get(ctx, keep)
	if err != nil || !m.Deleted {
		t.Fatalf("удаление из второго чата потерялось: %+v %v", m, err)
	}
	if _, err := st.Get(ctx, drop); err == nil {
		t.Fatal("дубль остался")
	}
	var n int
	st.db.QueryRowContext(ctx, `SELECT count(*) FROM reactions WHERE msg = ? AND emoji = '🔥'`, keep).Scan(&n)
	if n != 1 {
		t.Fatalf("реакция не переехала: %d", n)
	}
	if left, _ := st.History(ctx, a, Query{Limit: 10}); len(left) != 0 {
		t.Fatalf("отложенное удаление не применилось при слиянии: %+v", left)
	}
	// номер b больше никому не достаётся
	c, _ := st.ChatID(ctx, "g@g.us", "group")
	if c == b || c == a {
		t.Fatalf("номер слитого чата достался новому: %d", c)
	}
	// старый номер в записи — сообщение в рабочем чате
	id, _, err := st.Put(ctx, &Message{Chat: b, WAID: "LATE", Time: t0.Add(time.Hour), Text: "по старому номеру", Live: true})
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := st.Get(ctx, id); m == nil || m.Chat != a {
		t.Fatalf("запись по старому номеру: %+v", m)
	}
}

// Удалено в остающемся чате, а во втором — правка новее: после слияния
// сообщение остаётся удалённым и без текста.
func TestStoreMergeDeletedWins(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	a, _ := st.ChatID(ctx, "111@lid", "user")
	b, _ := st.ChatID(ctx, "7999@s.whatsapp.net", "user")
	keep, _, _ := st.Put(ctx, &Message{Chat: a, WAID: "D", Time: t0, Text: "одно"})
	st.Put(ctx, &Message{Chat: b, WAID: "D", Time: t0, Text: "одно"})
	st.Delete(ctx, a, "D", t0.Add(time.Minute))
	st.Edit(ctx, b, "D", "правка новее", nil, t0.Add(2*time.Minute))
	st.ChatID(ctx, "7999@s.whatsapp.net", "user", "111@lid")
	if m, _ := st.Get(ctx, keep); !m.Deleted || m.Text != "" {
		t.Fatalf("удалённое ожило: %+v", m)
	}
}

// Отложенное, не применённое прошлым процессом, применяет открытие базы.
func TestStorePendingRecovered(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "messages.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	chat, _ := st.ChatID(ctx, "g@g.us", "group")
	id, _, _ := st.Put(ctx, &Message{Chat: chat, WAID: "M", Time: t0, Text: "было"})
	// как будто процесс упал между записью сообщения и применением отложенного
	st.db.ExecContext(ctx, `INSERT INTO pending(chat, wa_id, kind, value, ts) VALUES(?, 'M', 'edit', 'стало', ?)`, chat, t0.Add(time.Minute).UnixMilli())
	st.Close()
	st, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if m, _ := st.Get(ctx, id); m.Text != "стало" {
		t.Fatalf("отложенное не применилось при открытии: %q", m.Text)
	}
	var n int
	st.db.QueryRowContext(ctx, `SELECT count(*) FROM pending`).Scan(&n)
	if n != 0 {
		t.Fatalf("отложенное осталось: %d", n)
	}
}

// Применить отложенное не вышло (ошибка базы) — следующая запись повторяет.
func TestStorePendingRetried(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	chat, _ := st.ChatID(ctx, "g@g.us", "group")
	st.Delete(ctx, chat, "M", t0.Add(time.Minute))
	// применение упадёт: удалить строку pending запрещено
	st.db.ExecContext(ctx, `CREATE TRIGGER no_drop BEFORE DELETE ON pending BEGIN SELECT RAISE(ABORT, 'занято'); END`)
	id, _, _ := st.Put(ctx, &Message{Chat: chat, WAID: "M", Time: t0, Text: "было"})
	if !st.owed {
		t.Fatal("сбой применения не запомнен")
	}
	st.db.ExecContext(ctx, `DROP TRIGGER no_drop`)
	st.Put(ctx, &Message{Chat: chat, WAID: "N", Time: t0, Text: "другое"})
	if st.owed {
		t.Fatal("долг не вернули")
	}
	if m, _ := st.Get(ctx, id); !m.Deleted {
		t.Fatalf("удаление так и не применилось: %+v", m)
	}
	var n int
	st.db.QueryRowContext(ctx, `SELECT count(*) FROM pending`).Scan(&n)
	if n != 0 {
		t.Fatalf("отложенное осталось: %d", n)
	}
}

// Не вышло записать правку сразу — она ждёт в pending и применяется
// следующей записью.
func TestStoreDirectFailureOwed(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	chat, _ := st.ChatID(ctx, "g@g.us", "group")
	id, _, _ := st.Put(ctx, &Message{Chat: chat, WAID: "M", Time: t0, Text: "было"})
	st.db.ExecContext(ctx, `CREATE TRIGGER no_edit BEFORE UPDATE OF text ON messages BEGIN SELECT RAISE(ABORT, 'занято'); END`)
	st.Edit(ctx, chat, "M", "стало", nil, t0.Add(time.Minute))
	if !st.owed {
		t.Fatal("сбой записи не запомнен")
	}
	st.db.ExecContext(ctx, `DROP TRIGGER no_edit`)
	st.Put(ctx, &Message{Chat: chat, WAID: "N", Time: t0, Text: "другое"})
	if m, _ := st.Get(ctx, id); m.Text != "стало" || st.owed {
		t.Fatalf("правка не дошла: %q, долг %v", m.Text, st.owed)
	}
}

// Не вышло даже положить изменение в pending — оно ждёт в памяти и
// доходит следующей записью.
func TestStoreUnsavedKept(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	chat, _ := st.ChatID(ctx, "g@g.us", "group")
	id, _, _ := st.Put(ctx, &Message{Chat: chat, WAID: "M", Time: t0, Text: "было"})
	st.db.ExecContext(ctx, `CREATE TRIGGER no_del BEFORE UPDATE OF deleted ON messages BEGIN SELECT RAISE(ABORT, 'занято'); END`)
	st.db.ExecContext(ctx, `CREATE TRIGGER no_pend BEFORE INSERT ON pending BEGIN SELECT RAISE(ABORT, 'занято'); END`)
	st.Delete(ctx, chat, "M", t0.Add(time.Minute))
	if len(st.unsaved) != 1 || !st.owed {
		t.Fatalf("изменение не удержано: %d, долг %v", len(st.unsaved), st.owed)
	}
	st.db.ExecContext(ctx, `DROP TRIGGER no_del`)
	st.db.ExecContext(ctx, `DROP TRIGGER no_pend`)
	st.Put(ctx, &Message{Chat: chat, WAID: "N", Time: t0, Text: "другое"})
	if m, _ := st.Get(ctx, id); !m.Deleted || st.owed || len(st.unsaved) != 0 {
		t.Fatalf("удаление не дошло: %+v, долг %v", m, st.owed)
	}
}

// Отменённый контекст вызова не теряет изменение и не кладёт его под
// слитый номер чата.
func TestStoreCanceledContext(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	a, _ := st.ChatID(ctx, "111@lid", "user")
	b, _ := st.ChatID(ctx, "7999@s.whatsapp.net", "user")
	id, _, _ := st.Put(ctx, &Message{Chat: a, WAID: "M", Time: t0, Text: "было"})
	st.ChatID(ctx, "111@lid", "user", "7999@s.whatsapp.net") // b слит в a
	gone, cancel := context.WithCancel(ctx)
	cancel()
	st.Delete(gone, b, "M", t0.Add(time.Minute))
	if m, _ := st.Get(ctx, id); !m.Deleted {
		t.Fatalf("удаление с отменённым контекстом потеряно: %+v", m)
	}
}

// Застрявшая правка и новая с тем же временем: повтор не возвращает старую.
func TestStoreOwedOrder(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	chat, _ := st.ChatID(ctx, "g@g.us", "group")
	id, _, _ := st.Put(ctx, &Message{Chat: chat, WAID: "M", Time: t0, Text: "было"})
	st.db.ExecContext(ctx, `CREATE TRIGGER no_edit BEFORE UPDATE OF text ON messages BEGIN SELECT RAISE(ABORT, 'занято'); END`)
	st.Edit(ctx, chat, "M", "первая", nil, t0.Add(time.Minute))
	st.db.ExecContext(ctx, `DROP TRIGGER no_edit`)
	st.Edit(ctx, chat, "M", "вторая", nil, t0.Add(time.Minute))
	st.Put(ctx, &Message{Chat: chat, WAID: "N", Time: t0, Text: "другое"})
	if m, _ := st.Get(ctx, id); m.Text != "вторая" || st.owed {
		t.Fatalf("порядок правок нарушен: %q, долг %v", m.Text, st.owed)
	}
}

// Из очереди в памяти в pending уходит только начало до первой неудачи:
// более позднее изменение не обгоняет застрявшее.
func TestStoreUnsavedPrefix(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	chat, _ := st.ChatID(ctx, "g@g.us", "group")
	id, _, _ := st.Put(ctx, &Message{Chat: chat, WAID: "M", Time: t0, Text: "было"})
	st.db.ExecContext(ctx, `CREATE TRIGGER no_edit BEFORE UPDATE OF text ON messages BEGIN SELECT RAISE(ABORT, 'занято'); END`)
	st.db.ExecContext(ctx, `CREATE TRIGGER no_pend BEFORE INSERT ON pending BEGIN SELECT RAISE(ABORT, 'занято'); END`)
	st.Edit(ctx, chat, "M", "первая", nil, t0.Add(time.Minute))
	st.Edit(ctx, chat, "M", "вторая", nil, t0.Add(time.Minute))
	// база ожила, но «первая» всё ещё не пишется
	st.db.ExecContext(ctx, `DROP TRIGGER no_edit`)
	st.db.ExecContext(ctx, `DROP TRIGGER no_pend`)
	st.db.ExecContext(ctx, `CREATE TRIGGER no_first BEFORE INSERT ON pending WHEN NEW.value = 'первая' BEGIN SELECT RAISE(ABORT, 'занято'); END`)
	st.Put(ctx, &Message{Chat: chat, WAID: "N", Time: t0, Text: "другое"})
	if m, _ := st.Get(ctx, id); m.Text != "было" || len(st.unsaved) != 2 {
		t.Fatalf("очередь обогнали: %q, в памяти %d", m.Text, len(st.unsaved))
	}
	st.db.ExecContext(ctx, `DROP TRIGGER no_first`)
	st.Put(ctx, &Message{Chat: chat, WAID: "O", Time: t0, Text: "ещё"})
	if m, _ := st.Get(ctx, id); m.Text != "вторая" || st.owed {
		t.Fatalf("порядок нарушен: %q, долг %v", m.Text, st.owed)
	}
}
