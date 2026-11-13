//go:build windows

package core

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/gotd/td/tg"

	"tgagent/internal/config"
	"tgagent/internal/inbox"
	"tgagent/internal/t3"
)

// fakeSession — входящий канал сессии Claude Code: принимает соединения и
// отдаёт присланный текст уведомления.
var pipeSeq atomic.Int64 // часы Windows грубые: одно время у двух каналов

func fakeSession(t *testing.T) (inbox.Addr, <-chan string) {
	t.Helper()
	addr := inbox.Addr{Session: "s-" + t.Name(), Socket: fmt.Sprintf(`\\.\pipe\tg-agent-test-%d-%d`,
		time.Now().UnixNano(), pipeSeq.Add(1)), Token: "tok"}
	ln, err := winio.ListenPipe(addr.Socket, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan string, 10)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			sc := bufio.NewScanner(conn)
			var auth, msg struct {
				Type    string `json:"type"`
				Token   string `json:"token"`
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}
			if sc.Scan() {
				_ = json.Unmarshal(sc.Bytes(), &auth)
			}
			if sc.Scan() {
				_ = json.Unmarshal(sc.Bytes(), &msg)
			}
			conn.Close()
			if auth.Type != "auth" || auth.Token != "tok" || msg.Type != "user" {
				got <- "BAD"
				continue
			}
			got <- msg.Message.Content
		}
	}()
	return addr, got
}

func expectNote(t *testing.T, got <-chan string, want ...string) {
	t.Helper()
	select {
	case text := <-got:
		for _, w := range want {
			if !strings.Contains(text, w) {
				t.Fatalf("в уведомлении нет %q:\n%s", w, text)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("уведомление не пришло")
	}
}

func expectSilence(t *testing.T, got <-chan string) {
	t.Helper()
	select {
	case text := <-got:
		t.Fatalf("лишнее уведомление:\n%s", text)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestSubscribeNotifies(t *testing.T) {
	s := watchEnv(t)
	feed := "test"
	appendNewer(s, feed, []int{10})
	addr, got := fakeSession(t)
	ctx := WithAgent(context.Background(), &addr)

	if _, err := Subscribe(ctx, s, "test", SubOpts{After: 0, Topic: 0, First: false}); err != nil {
		t.Fatal(err)
	}
	sent := map[string]time.Time{}
	notifyOnce(ctx, s, sent)
	expectSilence(t, got) // подписка с текущего момента — старое не шлём

	appendNewer(s, feed, []int{11, 12})
	notifyOnce(ctx, s, sent)
	expectNote(t, got, "test", "новых: 2", "after_id=10")

	// своё сообщение не будит, но отметку сдвигает
	markOutgoing("test", 13)
	appendNewer(s, feed, []int{13})
	notifyOnce(ctx, s, sent)
	expectSilence(t, got)

	// чаще notifyGap не будим — копим и шлём одним сообщением
	appendNewer(s, feed, []int{14})
	if wait := notifyOnce(ctx, s, sent); wait <= 0 {
		t.Fatal("ожидалась пауза перед следующим уведомлением")
	}
	expectSilence(t, got)
	sent[addr.Key()] = time.Time{}
	notifyOnce(ctx, s, sent)
	expectNote(t, got, "новых: 1", "after_id=13")
}

func TestSubscribeAfterCatchesUp(t *testing.T) {
	s := watchEnv(t)
	appendNewer(s, "test", []int{10, 11, 12})
	addr, got := fakeSession(t)
	ctx := WithAgent(context.Background(), &addr)
	if _, err := Subscribe(ctx, s, "test", SubOpts{After: 10, Topic: 0, First: false}); err != nil {
		t.Fatal(err)
	}
	notifyOnce(ctx, s, map[string]time.Time{})
	expectNote(t, got, "новых: 2", "after_id=10")
}

func TestSubscribeFollowsResumedSession(t *testing.T) {
	s := watchEnv(t)
	feed := "test"
	appendNewer(s, feed, []int{10})
	addr, _ := fakeSession(t)
	gone := addr
	gone.Socket += "-closed" // канала нет: сессия закрыта
	ctx := WithAgent(context.Background(), &gone)
	if _, err := Subscribe(ctx, s, "test", SubOpts{After: 0, Topic: 0, First: false}); err != nil {
		t.Fatal(err)
	}
	appendNewer(s, feed, []int{11})
	notifyOnce(ctx, s, map[string]time.Time{})
	if sub := loadSubs(s)[addr.Key()]; sub == nil || sub.Dead.IsZero() {
		t.Fatal("недоступная сессия должна быть помечена")
	}

	// сессию возобновили: тот же id, новый канал — пропущенное приходит
	resumed, got := fakeSession(t)
	resumed.Session = addr.Session
	if err := Hello(s, resumed); err != nil {
		t.Fatal(err)
	}
	notifyOnce(ctx, s, map[string]time.Time{})
	expectNote(t, got, "новых: 1", "after_id=10")
	if sub := loadSubs(s)[addr.Key()]; sub == nil || !sub.Dead.IsZero() {
		t.Fatal("ответившая сессия должна быть живой")
	}
}

func TestSubscribeNeedsSessionAndReadableChat(t *testing.T) {
	s := watchEnv(t)
	if _, err := Subscribe(WithAgent(context.Background(), nil), s, "test", SubOpts{}); err == nil {
		t.Fatal("без канала сессии подписка невозможна")
	}
	addr, _ := fakeSession(t)
	if _, err := Subscribe(WithAgent(context.Background(), &addr), s, "nope", SubOpts{After: 0, Topic: 0}); err == nil {
		t.Fatal("чат вне списка")
	}
	if _, err := Unsubscribe(WithAgent(context.Background(), &addr), s, ""); err != nil {
		t.Fatal(err)
	}
}

func TestSubscribeWakesStoppedT3Session(t *testing.T) {
	s := watchEnv(t)
	feed := "test"
	appendNewer(s, feed, []int{10})
	addr, _ := fakeSession(t)
	addr.Socket += "-closed" // T3 Code остановил процесс сессии
	ctx := WithAgent(context.Background(), &addr)
	if _, err := Subscribe(ctx, s, "test", SubOpts{After: 0, Topic: 0, First: false}); err != nil {
		t.Fatal(err)
	}
	var woken []string
	wakeT3 = func(_ context.Context, _ *config.Settings, session, text string) error {
		woken = append(woken, session+": "+text)
		return nil
	}
	sent := map[string]time.Time{}
	appendNewer(s, feed, []int{11})
	notifyOnce(ctx, s, sent)
	if len(woken) != 1 || !strings.Contains(woken[0], addr.Session) || !strings.Contains(woken[0], "after_id=10") {
		t.Fatalf("ожидалось пробуждение через T3: %q", woken)
	}
	if sub := loadSubs(s)[addr.Key()]; sub == nil || !sub.Dead.IsZero() || sub.Chats["test"] != 11 {
		t.Fatalf("после пробуждения через T3 отметка должна сдвинуться: %+v", sub)
	}

	// пока сессия поднимается, T3 повторно не дёргаем — копим
	appendNewer(s, feed, []int{12})
	sent[addr.Key()] = time.Time{}
	notifyOnce(ctx, s, sent)
	if len(woken) != 1 {
		t.Fatalf("лишнее пробуждение через T3: %d", len(woken))
	}
}

func TestSubscribeReportsEditsAndDeletes(t *testing.T) {
	s := watchEnv(t)
	feed := "test"
	appendNewer(s, feed, []int{10})
	addr, got := fakeSession(t)
	ctx := WithAgent(context.Background(), &addr)
	if _, err := Subscribe(ctx, s, "test", SubOpts{After: 0, Topic: 0, First: false}); err != nil {
		t.Fatal(err)
	}
	sent := map[string]time.Time{}
	appendNewer(s, feed, []int{11, 12})
	notifyOnce(ctx, s, sent)
	expectNote(t, got, "новых: 2") // агент знает о 11 и 12

	loader := func() (*config.Settings, error) { return s, nil }
	edit := FeedOnEdit(loader)
	edit(ctx, nil, &tg.PeerChat{ChatID: 100123}, 11, false) // правка уже известного
	edit(ctx, nil, &tg.PeerChat{ChatID: 100123}, 12, true)  // своя правка — не будит
	edit(ctx, nil, &tg.PeerChat{ChatID: 100123}, 99, false) // ещё не известное — придёт как новое
	// удаление в обычной группе: Telegram не говорит где — находим по ленте
	FeedOnDelete(loader)(ctx, nil, nil, []int{12, 5000})

	sent[addr.Key()] = time.Time{}
	notifyOnce(ctx, s, sent)
	expectNote(t, got, "изменены сообщения 11", "ids=[11]", "удалены сообщения 12")
	sub := loadSubs(s)[addr.Key()]
	if len(sub.Edited) != 0 || len(sub.Deleted) != 0 {
		t.Fatalf("после уведомления правки сброшены: %+v %+v", sub.Edited, sub.Deleted)
	}

	// правка удалённого не воскрешает его
	FeedOnDelete(loader)(ctx, nil, nil, []int{11})
	edit(ctx, nil, &tg.PeerChat{ChatID: 100123}, 11, false)
	sub = loadSubs(s)[addr.Key()]
	if len(sub.Edited) != 0 || len(sub.Deleted["test"]) != 1 {
		t.Fatalf("удалённое не правится: %+v %+v", sub.Edited, sub.Deleted)
	}
}

func TestSubscribeTopics(t *testing.T) {
	s := watchEnv(t)
	feed := "test"
	appendNewer(s, feed, []int{10})
	addr, got := fakeSession(t)
	ctx := WithAgent(context.Background(), &addr)
	// форум: 11 и 13 — в теме «Заказы» (5), 12 — в General
	where := map[int]topicRef{11: {5, "Заказы"}, 12: {1, "General"}, 13: {5, "Заказы"}, 14: {1, "General"}}
	oldOf, oldCheck := topicsOf, checkTopic
	t.Cleanup(func() { topicsOf, checkTopic = oldOf, oldCheck })
	topicsOf = func(_ context.Context, _ *config.Settings, _ config.ChatRule, ids []int) map[int]topicRef {
		out := map[int]topicRef{}
		for _, id := range ids {
			out[id] = where[id]
		}
		memoTopics("test", out)
		return out
	}
	checkTopic = func(_ context.Context, _ *config.Settings, _ config.ChatRule, topic int) (string, error) {
		if topic != 5 {
			return "", &Bad{Msg: "нет темы"}
		}
		return "Заказы", nil
	}

	if _, err := Subscribe(ctx, s, "test", SubOpts{After: 0, Topic: 0, First: false}); err != nil {
		t.Fatal(err)
	}
	sent := map[string]time.Time{}
	appendNewer(s, feed, []int{11, 12, 13})
	notifyOnce(ctx, s, sent)
	expectNote(t, got, "новых: 3", "«Заказы» (topic=5) — 2", "«General» (topic=1) — 1")

	// подписка на одну тему: General не будит, правки чужой темы — тоже
	if _, err := Subscribe(ctx, s, "test", SubOpts{After: 0, Topic: 7, First: false}); err == nil {
		t.Fatal("несуществующая тема")
	}
	out, err := Subscribe(ctx, s, "test", SubOpts{After: 0, Topic: 5, First: false})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := out.Get("topic"); v == nil {
		t.Fatal("в ответе подписки нет темы")
	}
	sent[addr.Key()] = time.Time{}
	appendNewer(s, feed, []int{14})
	FeedOnEdit(func() (*config.Settings, error) { return s, nil })(ctx, nil, &tg.PeerChat{ChatID: 100123}, 12, false)
	notifyOnce(ctx, s, sent)
	expectSilence(t, got)
	if sub := loadSubs(s)[addr.Key()]; sub.Chats["test"] != 14 || len(sub.Edited) != 0 {
		t.Fatalf("чужая тема: отметка сдвигается, правка сбрасывается: %+v", sub)
	}
	appendNewer(s, feed, []int{15})
	where[15] = topicRef{5, "Заказы"}
	sent[addr.Key()] = time.Time{}
	notifyOnce(ctx, s, sent)
	expectNote(t, got, "тема «Заказы» (topic=5) — новых: 1", "topic=5, after_id=14")

	// отписка от чата снимает и тему
	if _, err := Unsubscribe(ctx, s, "test"); err != nil {
		t.Fatal(err)
	}
	if sub := loadSubs(s)[addr.Key()]; sub != nil {
		t.Fatalf("подписка осталась: %+v", sub)
	}
}

func TestSubscriptionsFollowNewT3Session(t *testing.T) {
	s := watchEnv(t)
	feed := "test"
	appendNewer(s, feed, []int{10})
	threadOf = func(_ context.Context, session string) (string, error) {
		if session == "old-s" || session == "new-s" {
			return "th", nil
		}
		return "", t3.NoThread
	}
	currentSession = func(context.Context, string) string { return "new-s" }

	// подписалась прежняя сессия треда; T3 завёл треду новую
	old, _ := fakeSession(t)
	old.Session, old.Socket = "old-s", old.Socket+"-closed"
	if _, err := Subscribe(WithAgent(context.Background(), &old), s, "test", SubOpts{After: 0, Topic: 0}); err != nil {
		t.Fatal(err)
	}
	fresh, got := fakeSession(t)
	fresh.Session = "new-s"
	if err := Hello(s, fresh); err != nil {
		t.Fatal(err)
	}
	subs := loadSubs(s)
	if subs["old-s"] != nil || subs["new-s"] == nil || subs["new-s"].Chats["test"] != 10 || subs["new-s"].Agent != fresh {
		t.Fatalf("подписка должна перейти к новой сессии: %+v", subs)
	}
	appendNewer(s, feed, []int{11})
	notifyOnce(context.Background(), s, map[string]time.Time{})
	expectNote(t, got, "новых: 1", "after_id=10")

	// обе сессии подписаны (новая — сама): будим одну, текущую
	if _, err := Subscribe(WithAgent(context.Background(), &old), s, "test", SubOpts{After: 11, Topic: 0}); err != nil {
		t.Fatal(err)
	}
	appendNewer(s, feed, []int{12})
	notifyOnce(context.Background(), s, map[string]time.Time{})
	expectNote(t, got, "новых: 1", "after_id=11")
	expectSilence(t, got)
	if subs := loadSubs(s); len(subs) != 1 || subs["new-s"] == nil {
		t.Fatalf("у треда должна остаться одна подписка: %+v", subs)
	}
}

func TestSubscriptionsFollowCodexHandoff(t *testing.T) {
	for _, alreadySubscribed := range []bool{false, true} {
		t.Run(fmt.Sprintf("subscribed=%v", alreadySubscribed), func(t *testing.T) {
			s := watchEnv(t)
			ctx := context.Background()
			rule := s.Chats["test"]
			rule.Send = true
			s.Chats["test"] = rule
			threadOf = func(_ context.Context, session string) (string, error) {
				if session == "claude-s" || session == "codex-s" {
					return "same-thread", nil
				}
				return "", t3.NoThread
			}
			currentSession = func(context.Context, string) string { return "codex-s" }
			old := inbox.Addr{Session: "claude-s", Socket: "closed", Token: "test-token"}
			fresh := inbox.Addr{Session: "codex-s", Kind: inbox.KindCodex}
			appendNewer(s, "test", []int{90, 91, 92})
			if _, err := Subscribe(WithAgent(ctx, &old), s, "test", SubOpts{After: 90}); err != nil {
				t.Fatal(err)
			}
			if err := editRoutes(s, func(rs routes) bool {
				rs["test"] = map[int]*route{90: {Chain: []string{old.Key()}, Told: true, At: time.Now()}}
				return true
			}); err != nil {
				t.Fatal(err)
			}
			if alreadySubscribed {
				if _, err := Subscribe(WithAgent(ctx, &fresh), s, "test", SubOpts{After: 92}); err != nil {
					t.Fatal(err)
				}
			}
			if err := Hello(s, fresh); err != nil {
				t.Fatal(err)
			}
			want := 90
			if alreadySubscribed {
				want = 92 // the new provider has already processed these messages
			}
			subs := loadSubs(s)
			if len(subs) != 1 || subs[old.Key()] != nil || subs[fresh.Key()] == nil ||
				subs[fresh.Key()].Chats["test"] != want || subs[fresh.Key()].Agent != fresh {
				t.Fatalf("handoff lost the subscription, cursor or provider: %+v", subs)
			}
			if r := loadRoutes(s)["test"][90]; r.owner() != fresh.Key() || !r.Told {
				t.Fatalf("handoff lost existing message ownership: %+v", r)
			}
			appendNewer(s, "test", []int{93})
			calls := 0
			wakeT3 = func(_ context.Context, _ *config.Settings, session, text string) error {
				calls++
				if session != fresh.Session || !strings.Contains(text, fmt.Sprintf("after_id=%d", want)) ||
					!strings.Contains(text, fmt.Sprintf("новых: %d", 93-want)) {
					t.Errorf("wrong handoff notification: %s %s", session, text)
				}
				return nil
			}
			notifyOnce(ctx, s, map[string]time.Time{})
			if calls != 1 {
				t.Fatalf("wanted one Codex notification, got %d", calls)
			}
			if got := loadSubs(s)[fresh.Key()].Chats["test"]; got != 93 {
				t.Fatalf("Codex was not notified: cursor %d", got)
			}
		})
	}
}

func TestSubscribeQueue(t *testing.T) {
	s := watchEnv(t)
	rule := s.Chats["test"]
	rule.Send = true // куда можно писать — у сообщения один владелец
	s.Chats["test"] = rule
	feed := "test"
	appendNewer(s, feed, []int{10})
	texts := map[int]*msgMeta{
		11: {FromID: "1", Text: "привет"},
		12: {FromID: "1", Text: "Выставь СЧЁТ, пожалуйста"},
		13: {FromID: "2", Username: "ivan", Text: "счёт за октябрь"},
	}
	oldMeta := metaOf
	t.Cleanup(func() { metaOf = oldMeta })
	metaOf = func(_ context.Context, _ *config.Settings, _ config.ChatRule, ids []int) map[int]*msgMeta {
		out := map[int]*msgMeta{}
		for _, id := range ids {
			out[id] = texts[id]
		}
		return out
	}
	threadInfo = func(_ context.Context, session string) (t3.SessionThread, error) {
		return t3.SessionThread{ID: "th-" + session, Title: "тред " + session}, nil
	}
	a, gotA := fakeSession(t)
	a.Session = "a"
	b, gotB := fakeSession(t)
	b.Session = "b"
	c, gotC := fakeSession(t)
	c.Session = "c"
	ctxA, ctxB, ctxC := WithAgent(context.Background(), &a), WithAgent(context.Background(), &b), WithAgent(context.Background(), &c)

	// широкая подписка не мешает другим: чат больше не «занят»
	if _, err := Subscribe(ctxA, s, "test", SubOpts{}); err != nil {
		t.Fatal(err)
	}
	out, err := Subscribe(ctxB, s, "test", SubOpts{Text: []string{"счёт"}})
	if err != nil {
		t.Fatalf("вторая подписка на чат: %v", err)
	}
	if q, _ := out.Get("queue"); q == nil {
		t.Fatal("в ответе нет очереди")
	}
	time.Sleep(10 * time.Millisecond) // c подписан позже b
	if _, err := Subscribe(ctxC, s, "test", SubOpts{From: []string{"@Ivan"}}); err != nil {
		t.Fatal(err)
	}
	sent := map[string]time.Time{}

	// не по словам b и не от ivan — широкой a
	appendNewer(s, feed, []int{11})
	notifyOnce(context.Background(), s, sent)
	expectNote(t, gotA, "тебе: 1 (id 11)")
	expectSilence(t, gotB)
	expectSilence(t, gotC)

	// «счёт» — узкой b раньше широкой a; ё = е, регистр неважен
	clear(sent)
	appendNewer(s, feed, []int{12})
	notifyOnce(context.Background(), s, sent)
	expectNote(t, gotB, "тебе: 1 (id 12)", "tg_pass")
	expectSilence(t, gotA)

	// чужое передать нельзя
	if _, err := Pass(ctxA, s, "test", 12, ""); err == nil || !strings.Contains(err.Error(), "не твоё") {
		t.Fatalf("не владелец не передаёт: %v", err)
	}
	// b передаёт — следующей в цепочке, a, с запиской
	res, err := Pass(ctxB, s, "test", 12, "это не про оплату")
	if err != nil {
		t.Fatal(err)
	}
	if to, _ := res.Get("passed_to"); to == nil {
		t.Fatalf("некому передать: %v", res)
	}
	clear(sent)
	notifyOnce(context.Background(), s, sent)
	expectNote(t, gotA, "передано тебе сообщение 12", "это не про оплату")
	expectSilence(t, gotB)
	// a передаёт — дальше никого
	res, err = Pass(ctxA, s, "test", 12, "")
	if err != nil {
		t.Fatal(err)
	}
	if to, _ := res.Get("passed_to"); to != nil {
		t.Fatalf("цепочка кончилась, а передано %v", to)
	}

	// подходит двум узким — первая по времени подписки (b), потом c, потом a
	clear(sent)
	appendNewer(s, feed, []int{13})
	notifyOnce(context.Background(), s, sent)
	expectNote(t, gotB, "id 13")
	expectSilence(t, gotC)
	expectSilence(t, gotA)
	if _, err := Pass(ctxB, s, "test", 13, ""); err != nil {
		t.Fatal(err)
	}
	clear(sent)
	notifyOnce(context.Background(), s, sent)
	expectNote(t, gotC, "передано тебе сообщение 13")
	expectSilence(t, gotA)

	// правка сообщения — только его владельцу
	noteChange(s, "test", []int{13}, false)
	clear(sent)
	notifyOnce(context.Background(), s, sent)
	expectNote(t, gotC, "изменены сообщения 13")
	expectSilence(t, gotB)

	// тред c закрыт — новая подписка снимает его, его сообщения не теряются
	threadInfo = func(_ context.Context, session string) (t3.SessionThread, error) {
		return t3.SessionThread{ID: "th-" + session, Title: "тред " + session, Closed: session == "c"}, nil
	}
	if _, err := Subscribe(ctxA, s, "test", SubOpts{}); err != nil {
		t.Fatal(err)
	}
	if subs := loadSubs(s); subs["c"] != nil {
		t.Fatalf("подписка закрытого треда снята: %+v", subs["c"])
	}
}

func TestSubscribeReadOnlyFilters(t *testing.T) {
	s := watchEnv(t) // чат test — только чтение: маршрутов нет, каждому своё
	feed := "test"
	appendNewer(s, feed, []int{10})
	oldMeta := metaOf
	t.Cleanup(func() { metaOf = oldMeta })
	metaOf = func(_ context.Context, _ *config.Settings, _ config.ChatRule, ids []int) map[int]*msgMeta {
		out := map[int]*msgMeta{}
		for _, id := range ids {
			out[id] = &msgMeta{FromID: "7", Text: map[int]string{11: "заказ 5", 12: "привет"}[id]}
		}
		return out
	}
	a, gotA := fakeSession(t)
	a.Session = "a"
	b, gotB := fakeSession(t)
	b.Session = "b"
	if _, err := Subscribe(WithAgent(context.Background(), &a), s, "test", SubOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Subscribe(WithAgent(context.Background(), &b), s, "test", SubOpts{Text: []string{"заказ"}}); err != nil {
		t.Fatal(err)
	}
	appendNewer(s, feed, []int{11, 12})
	notifyOnce(context.Background(), s, map[string]time.Time{})
	expectNote(t, gotA, "тебе: 2 (id 11, 12)")
	expectNote(t, gotB, "тебе: 1 (id 11)")
}

func TestSubscribeTakesFromDeadSession(t *testing.T) {
	s := watchEnv(t)
	feed := "test"
	appendNewer(s, feed, []int{10})
	a, _ := fakeSession(t)
	a.Session, a.Socket = "s-a", a.Socket+"-closed"
	b, _ := fakeSession(t)
	b.Session = "s-b"
	if _, err := Subscribe(WithAgent(context.Background(), &a), s, "test", SubOpts{}); err != nil {
		t.Fatal(err)
	}
	appendNewer(s, feed, []int{11})
	notifyOnce(context.Background(), s, map[string]time.Time{}) // a не отвечает — Dead
	out, err := Subscribe(WithAgent(context.Background(), &b), s, "test", SubOpts{})
	if err != nil {
		t.Fatalf("подписка рядом с мёртвой сессией: %v", err)
	}
	if v, _ := out.Get("taken_from"); v == nil {
		t.Fatal("в ответе нет taken_from")
	}
}
