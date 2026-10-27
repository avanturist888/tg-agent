//go:build live

package maxc

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestLiveSession — вход по сохранённой сессии владельца и минута с лишним
// на связи: пинги проходят, события разбираются. Пишет только числа.
// go test -tags live -run Live ./internal/maxc -v
func TestLiveSession(t *testing.T) {
	home, _ := os.Getwd()
	path := filepath.Join(home, "..", "..", "data", "max", "session.json")
	sess, err := LoadSession(path)
	if err != nil {
		t.Skip(err)
	}
	var mu sync.Mutex
	ops := map[uint16]int{}
	bad := 0
	ctx := context.Background()
	c, r, err := Open(ctx, sess, Options{OnEvent: func(f Frame) {
		var v any
		mu.Lock()
		defer mu.Unlock()
		ops[f.Opcode]++
		if f.Decode(&v) != nil {
			bad++
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	t.Logf("вход: профиль есть %v, чатов %d, контактов %d", r.Profile.Contact.ID != 0, len(r.Chats), len(r.Contacts))
	select {
	case <-c.Done():
		t.Fatalf("соединение закрылось: %v", c.Err())
	case <-time.After(75 * time.Second):
	}
	mu.Lock()
	defer mu.Unlock()
	t.Logf("события по опкодам: %v, не разобрано: %d", ops, bad)
	if bad > 0 {
		t.Fatal("есть неразобранные события")
	}
}

// TestLiveSaved — запись в «Избранном» (чат 0): отправить, исправить,
// найти в истории, удалить у всех. Больше нигде не пишет.
// go test -tags live -run LiveSaved ./internal/maxc -v
func TestLiveSaved(t *testing.T) {
	home, _ := os.Getwd()
	sess, err := LoadSession(filepath.Join(home, "..", "..", "data", "max", "session.json"))
	if err != nil {
		t.Skip(err)
	}
	ctx := context.Background()
	evs := make(chan Event, 100)
	cl := &Client{Session: sess, OnEvent: func(e Event) { evs <- e }}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go cl.Run(cctx)
	wctx, wcancel := context.WithTimeout(ctx, time.Minute)
	defer wcancel()
	c, err := cl.Conn(wctx)
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.Send(ctx, SavedChat, "проба tg-agent (Go)", SendOptions{Silent: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("отправлено: id есть %v, cid совпал %v", m.ID != 0, m.CID != 0)
	ed, err := c.Edit(ctx, SavedChat, m.ID, "проба tg-agent (Go), исправлено", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("исправлено: статус %s", ed.Status)
	hist, err := c.History(ctx, SavedChat, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hist {
		if h.ID == m.ID {
			found = h.Text == "проба tg-agent (Go), исправлено"
		}
	}
	t.Logf("история: %d сообщений, проба с правкой найдена %v", len(hist), found)
	if err := c.Delete(ctx, SavedChat, []int64{m.ID}, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	kinds := map[EventKind]int{}
	for len(evs) > 0 {
		kinds[(<-evs).Kind]++
	}
	t.Logf("удалено; события за это время по видам: %v", kinds)
	if !found {
		t.Fatal("правка не видна в истории")
	}
}

// TestLiveListen — слушать события MAX_LISTEN_MIN минут (по умолчанию 10) и
// писать их вид, без текста: проверка разбора на живых событиях.
// go test -tags live -run LiveListen ./internal/maxc -v -timeout 30m
func TestLiveListen(t *testing.T) {
	home, _ := os.Getwd()
	sess, err := LoadSession(filepath.Join(home, "..", "..", "data", "max", "session.json"))
	if err != nil {
		t.Skip(err)
	}
	minutes := 10
	if v, err := strconv.Atoi(os.Getenv("MAX_LISTEN_MIN")); err == nil && v > 0 {
		minutes = v
	}
	names := map[EventKind]string{EventOther: "другое", EventNew: "новое", EventEdit: "правка",
		EventDelete: "удаление", EventRead: "прочтение", EventPresence: "в сети"}
	cl := &Client{Session: sess, OnEvent: func(e Event) {
		if e.Kind == EventPresence {
			return
		}
		text := -1
		if e.Message != nil {
			text = len([]rune(e.Message.Text))
		}
		t.Logf("%s: opcode %d, избранное %v, сообщений %d, длина текста %d",
			names[e.Kind], e.Frame.Opcode, e.ChatID == SavedChat, max(len(e.MessageIDs), btoi(e.Message != nil)), text)
	}, OnDisconnect: func(err error) { t.Logf("обрыв: %v", err) }}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(minutes)*time.Minute)
	defer cancel()
	t.Logf("слушаю %d мин", minutes)
	_ = cl.Run(ctx)
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
