//go:build windows

package core

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/gotd/td/tg"

	"tgagent/internal/config"
	"tgagent/internal/inbox"
)

// fakeSession — входящий канал сессии Claude Code: принимает соединения и
// отдаёт присланный текст уведомления.
func fakeSession(t *testing.T) (inbox.Addr, <-chan string) {
	t.Helper()
	addr := inbox.Addr{Session: "s-" + t.Name(), Socket: fmt.Sprintf(`\\.\pipe\tg-agent-test-%d`, time.Now().UnixNano()), Token: "tok"}
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
	feed := FeedPath(s, "test")
	appendNewer(feed, []int{10})
	addr, got := fakeSession(t)
	ctx := WithAgent(context.Background(), &addr)

	if _, err := Subscribe(ctx, s, "test", 0); err != nil {
		t.Fatal(err)
	}
	sent := map[string]time.Time{}
	notifyOnce(ctx, s, sent)
	expectSilence(t, got) // подписка с текущего момента — старое не шлём

	appendNewer(feed, []int{11, 12})
	notifyOnce(ctx, s, sent)
	expectNote(t, got, "test", "новых: 2", "after_id=10")

	// своё сообщение не будит, но отметку сдвигает
	markOutgoing("test", 13)
	appendNewer(feed, []int{13})
	notifyOnce(ctx, s, sent)
	expectSilence(t, got)

	// чаще notifyGap не будим — копим и шлём одним сообщением
	appendNewer(feed, []int{14})
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
	appendNewer(FeedPath(s, "test"), []int{10, 11, 12})
	addr, got := fakeSession(t)
	ctx := WithAgent(context.Background(), &addr)
	if _, err := Subscribe(ctx, s, "test", 10); err != nil {
		t.Fatal(err)
	}
	notifyOnce(ctx, s, map[string]time.Time{})
	expectNote(t, got, "новых: 2", "after_id=10")
}

func TestSubscribeFollowsResumedSession(t *testing.T) {
	s := watchEnv(t)
	feed := FeedPath(s, "test")
	appendNewer(feed, []int{10})
	addr, _ := fakeSession(t)
	gone := addr
	gone.Socket += "-closed" // канала нет: сессия закрыта
	ctx := WithAgent(context.Background(), &gone)
	if _, err := Subscribe(ctx, s, "test", 0); err != nil {
		t.Fatal(err)
	}
	appendNewer(feed, []int{11})
	notifyOnce(ctx, s, map[string]time.Time{})
	if sub := loadSubs(subsPath(s))[addr.Key()]; sub == nil || sub.Dead.IsZero() {
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
	if sub := loadSubs(subsPath(s))[addr.Key()]; sub == nil || !sub.Dead.IsZero() {
		t.Fatal("ответившая сессия должна быть живой")
	}
}

func TestSubscribeNeedsSessionAndReadableChat(t *testing.T) {
	s := watchEnv(t)
	if _, err := Subscribe(WithAgent(context.Background(), nil), s, "test", 0); err == nil {
		t.Fatal("без канала сессии подписка невозможна")
	}
	addr, _ := fakeSession(t)
	if _, err := Subscribe(WithAgent(context.Background(), &addr), s, "nope", 0); err == nil {
		t.Fatal("чат вне списка")
	}
	if _, err := Unsubscribe(WithAgent(context.Background(), &addr), s, ""); err != nil {
		t.Fatal(err)
	}
}

func TestSubscribeWakesStoppedT3Session(t *testing.T) {
	s := watchEnv(t)
	feed := FeedPath(s, "test")
	appendNewer(feed, []int{10})
	addr, _ := fakeSession(t)
	addr.Socket += "-closed" // T3 Code остановил процесс сессии
	ctx := WithAgent(context.Background(), &addr)
	if _, err := Subscribe(ctx, s, "test", 0); err != nil {
		t.Fatal(err)
	}
	var woken []string
	wakeT3 = func(_ context.Context, _ *config.Settings, session, text string) error {
		woken = append(woken, session+": "+text)
		return nil
	}
	sent := map[string]time.Time{}
	appendNewer(feed, []int{11})
	notifyOnce(ctx, s, sent)
	if len(woken) != 1 || !strings.Contains(woken[0], addr.Session) || !strings.Contains(woken[0], "after_id=10") {
		t.Fatalf("ожидалось пробуждение через T3: %q", woken)
	}
	if sub := loadSubs(subsPath(s))[addr.Key()]; sub == nil || !sub.Dead.IsZero() || sub.Chats["test"] != 11 {
		t.Fatalf("после пробуждения через T3 отметка должна сдвинуться: %+v", sub)
	}

	// пока сессия поднимается, T3 повторно не дёргаем — копим
	appendNewer(feed, []int{12})
	sent[addr.Key()] = time.Time{}
	notifyOnce(ctx, s, sent)
	if len(woken) != 1 {
		t.Fatalf("лишнее пробуждение через T3: %d", len(woken))
	}
}

func TestSubscribeReportsEditsAndDeletes(t *testing.T) {
	s := watchEnv(t)
	feed := FeedPath(s, "test")
	appendNewer(feed, []int{10})
	addr, got := fakeSession(t)
	ctx := WithAgent(context.Background(), &addr)
	if _, err := Subscribe(ctx, s, "test", 0); err != nil {
		t.Fatal(err)
	}
	sent := map[string]time.Time{}
	appendNewer(feed, []int{11, 12})
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
	sub := loadSubs(subsPath(s))[addr.Key()]
	if len(sub.Edited) != 0 || len(sub.Deleted) != 0 {
		t.Fatalf("после уведомления правки сброшены: %+v %+v", sub.Edited, sub.Deleted)
	}

	// правка удалённого не воскрешает его
	FeedOnDelete(loader)(ctx, nil, nil, []int{11})
	edit(ctx, nil, &tg.PeerChat{ChatID: 100123}, 11, false)
	sub = loadSubs(subsPath(s))[addr.Key()]
	if len(sub.Edited) != 0 || len(sub.Deleted["test"]) != 1 {
		t.Fatalf("удалённое не правится: %+v %+v", sub.Edited, sub.Deleted)
	}
}
