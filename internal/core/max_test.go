package core

import (
	"context"
	"strings"
	"testing"

	"tgagent/internal/config"
	"tgagent/internal/maxc"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
)

// maxEnv — профиль MAX без сети: чаты «Избранное» (0) и группа (-9).
func maxEnv(t *testing.T) (*config.Settings, func() (*config.Settings, error)) {
	t.Helper()
	s := watchEnv(t)
	s.Network = config.NetMAX
	s.Chats = map[string]config.ChatRule{
		"saved": {Alias: "saved", Peer: int64(0), Read: true, Send: true},
		"team":  {Alias: "team", Peer: int64(-9), Read: true, Send: true},
	}
	s.ChatOrder = []string{"saved", "team"}
	maxRT.Lock()
	maxRT.self, maxRT.names = 100, map[int64]string{100: "Я", 5: "Аня"}
	maxRT.Unlock()
	t.Cleanup(func() {
		maxRT.Lock()
		maxRT.self, maxRT.names, maxRT.client = 0, nil, nil
		maxRT.Unlock()
	})
	return s, func() (*config.Settings, error) { return s, nil }
}

func TestMaxFeedsAndChanges(t *testing.T) {
	s, loader := maxEnv(t)
	ev := func(kind maxc.EventKind, chat int64, m *maxc.Message, ids ...int64) maxc.Event {
		return maxc.Event{Kind: kind, ChatID: chat, Message: m, MessageIDs: ids}
	}
	maxOnEvent(loader, ev(maxc.EventNew, -9, &maxc.Message{ID: 1000, Sender: 5}))
	maxOnEvent(loader, ev(maxc.EventNew, -9, &maxc.Message{ID: 1001, Sender: 100})) // своё
	maxOnEvent(loader, ev(maxc.EventNew, -77, &maxc.Message{ID: 5000, Sender: 5}))  // чат не в списке
	if got := LastID(FeedPath(s, "team")); got != 1001 {
		t.Fatalf("лента team: %d", got)
	}
	if !isOutgoing("team", 1001) || isOutgoing("team", 1000) {
		t.Fatal("своё сообщение должно быть помечено, чужое — нет")
	}
	if got := idsAfter(FeedPath(s, "team"), 0); len(got) != 2 {
		t.Fatalf("в ленте %v", got)
	}
	// в «Избранном» всё от владельца — и оно будит, как в Telegram
	maxOnEvent(loader, ev(maxc.EventNew, 0, &maxc.Message{ID: 2000, Sender: 100}))
	if LastID(FeedPath(s, "saved")) != 2000 || isOutgoing("saved", 2000) {
		t.Fatal("сообщение владельца в Избранном должно будить подписку")
	}

	// подписка видит правки и удаления уже известных сообщений
	sub := &subscription{Chats: map[string]int{"team": 1001}}
	if err := editSubs(s, func(m map[string]*subscription) bool { m["sess"] = sub; return true }); err != nil {
		t.Fatal(err)
	}
	maxOnEvent(loader, ev(maxc.EventEdit, -9, &maxc.Message{ID: 1000, Status: "EDITED"}))
	maxOnEvent(loader, ev(maxc.EventDelete, -9, nil, 1001))
	subs := loadSubs(subsPath(s))
	if got := subs["sess"]; got == nil || len(got.Edited["team"]) != 1 || got.Edited["team"][0] != 1000 ||
		len(got.Deleted["team"]) != 1 || got.Deleted["team"][0] != 1001 {
		t.Fatalf("правки и удаления: %+v", got)
	}
}

func TestMaxSerialize(t *testing.T) {
	maxEnv(t)
	m := maxc.Message{ID: 7, Time: 1792666937000, Sender: 5, Text: "", Status: "EDITED", UpdateTime: 1792666999000,
		Attaches: []map[string]any{{"_type": "PHOTO"}, {"_type": "CONTROL", "event": "joined"}},
		Link: &maxc.Link{Type: "FORWARD", Message: &maxc.Message{ID: 3, Sender: 100, Text: "исходное", Time: 1792666000000}}}
	got := maxSerialize(context.Background(), nil, m)
	want := `{"id":7,"date":"2026-10-22T`
	if s := compact(got); !strings.HasPrefix(s, want) || !strings.Contains(s, `"from":{"id":5,"name":"Аня"}`) ||
		!strings.Contains(s, `"text":"исходное"`) || !strings.Contains(s, `"forwarded_from":{"from":{"id":100,"name":"Я"}`) ||
		!strings.Contains(s, `"media":["PHOTO"]`) || !strings.Contains(s, `"service_action":"joined"`) ||
		!strings.Contains(s, `"edited":"`) {
		t.Fatalf("сообщение: %s", s)
	}
	reply := maxSerialize(context.Background(), nil, maxc.Message{ID: 8, Sender: 100, Text: "да",
		Link: &maxc.Link{Type: "REPLY", MessageID: 7, Message: &maxc.Message{ID: 7, Sender: 5, Text: "вопрос"}}})
	if s := compact(reply); !strings.Contains(s, `"reply_to":7`) || !strings.Contains(s, `"reply_to_message":{"id":7`) {
		t.Fatalf("ответ: %s", s)
	}
}

func TestMaxDraftLimits(t *testing.T) {
	s, _ := maxEnv(t)
	s.SendPolicy = "bot_approval"
	if _, err := DraftMessage(context.Background(), s, "team", "x", nil, "", "", []string{"C:/a.txt"}, false, 0); err == nil ||
		!strings.Contains(err.Error(), "только текст") {
		t.Fatalf("файлы в MAX: %v", err)
	}
	if _, err := DraftMessage(context.Background(), s, "team", "x", nil, "", "", nil, false, 5); err == nil {
		t.Fatal("темы в MAX нет")
	}
	text, els := maxText(&outbox.Draft{Text: "**да**", Fmt: "markdown"})
	if text != "да" || len(els) != 1 || els[0].Type != "STRONG" {
		t.Fatalf("markdown: %q %v", text, els)
	}
	if text, els = maxText(&outbox.Draft{Text: "**да**", Fmt: "plain"}); text != "**да**" || els != nil {
		t.Fatalf("plain: %q %v", text, els)
	}
	if _, err := maxReadChat(context.Background(), s, s.Chats["team"], ReadOpts{Search: "x"}, 10); err == nil {
		t.Fatal("поиска в MAX нет")
	}
	if _, err := maxReadChat(context.Background(), s, s.Chats["team"], ReadOpts{}, 10); err != ErrNoMax {
		t.Fatalf("без службы: %v", err)
	}
}

func compact(m *omap.Map) string {
	raw, _ := omap.Marshal(m)
	return string(raw)
}
