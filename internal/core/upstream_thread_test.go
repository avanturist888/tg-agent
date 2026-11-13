package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"tgagent/internal/config"
)

func TestUpstreamWakeThread(t *testing.T) {
	old, oldSend := config.Root, nestorSend
	config.Root = t.TempDir()
	t.Setenv("TG_NESTOR", "nestor-test")
	t.Cleanup(func() { config.Root, nestorSend = old, oldSend })
	var calls [][]string
	fail := map[string]bool{}
	sendErr := errors.New("тред закрыт")
	nestorSend = func(_ context.Context, exe, text string, args ...string) error {
		calls = append(calls, append([]string{text}, args...))
		if fail[args[1]] {
			return sendErr
		}
		return nil
	}
	s := &config.Settings{}

	// Без назначения не создаём ни тред, ни письмо другому агенту.
	if err := wakeProjectAgent(context.Background(), s, "Заголовок", "задание"); err == nil {
		t.Fatal("нет ошибки при отсутствующем назначении")
	}
	if len(calls) != 0 {
		t.Fatalf("отправка без назначения: %q", calls)
	}

	// назначенный — туда, с заголовком в тексте
	if err := SetUpstreamThread("mcp:abc"); err != nil {
		t.Fatal(err)
	}
	calls = nil
	if err := wakeProjectAgent(context.Background(), s, "Заголовок", "задание"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || strings.Join(calls[0][1:], " ") != "send mcp:abc -" || !strings.HasPrefix(calls[0][0], "«Заголовок»") {
		t.Fatalf("назначенный тред: %q", calls)
	}

	// Не дошло — ошибка остаётся видимой, никакого нового треда.
	fail["mcp:abc"] = true
	calls = nil
	if err := wakeProjectAgent(context.Background(), s, "Заголовок", "задание"); !errors.Is(err, sendErr) {
		t.Fatalf("ошибка доставки потеряна: %v", err)
	}
	if len(calls) != 1 || strings.Join(calls[0][1:], " ") != "send mcp:abc -" {
		t.Fatalf("адресат после сбоя: %q", calls)
	}

	for _, invalid := range []string{"", "   ", "new"} {
		if err := SetUpstreamThread(invalid); err == nil {
			t.Fatalf("принято пустое/новое назначение: %q", invalid)
		}
		if got := ReadUpstreamThread(); got == nil || got.Thread != "mcp:abc" {
			t.Fatalf("назначение потеряно: %+v", got)
		}
	}
	// После восстановления тот же тред получает повтор.
	fail["mcp:abc"] = false
	calls = nil
	if err := wakeProjectAgent(context.Background(), s, "Заголовок", "задание"); err != nil || len(calls) != 1 {
		t.Fatalf("повтор: %v, %q", err, calls)
	}
}

// Карточку, которую агент не разобрал за upstreamAgentWait, получает
// владелец: письмо в занятый тред могло не дойти.
func TestUpstreamAgentStale(t *testing.T) {
	f := &fakeUpstream{value: "26.10.6"}
	s := f.install(t)
	ctx := context.Background()
	upstreamWake = func(context.Context, *config.Settings, string, string) error { return nil }
	CheckUpstream(ctx, s)
	f.value = "26.11.0"
	CheckUpstream(ctx, s)
	f.take()
	escalateStale(ctx, s, time.Now().Add(time.Hour))
	if calls := f.take(); len(calls) != 0 {
		t.Fatalf("рано: %q", calls)
	}
	escalateStale(ctx, s, time.Now().Add(upstreamAgentWait+time.Minute))
	if calls := f.take(); strings.Join(calls, "|") != "editMessageText:2|pinChatMessage:2" ||
		!strings.Contains(f.lastEdit, "агент не разобрал") {
		t.Fatalf("владельцу: %q %q", calls, f.lastEdit)
	}
	if cards, _ := PinnedUpstream(s); len(cards) != 1 || cards[0].Agent {
		t.Fatalf("после: %+v", cards)
	}
	// неудачное закрепление без агента — изменение не запоминается
	f.value = "26.12.0"
	upstreamWake = func(context.Context, *config.Settings, string, string) error { return errors.New("нет") }
	f.mu.Lock()
	oldBot := upstreamBot
	f.mu.Unlock()
	upstreamBot = func(ctx context.Context, s *config.Settings, method string, p any, out any) error {
		if method == "pinChatMessage" {
			return errors.New("бот недоступен")
		}
		return oldBot(ctx, s, method, p, out)
	}
	if st, _ := CheckUpstream(ctx, s); st.Sources["web"].Values["v"] != "26.11.0" {
		t.Fatalf("запомнено, хотя владелец не увидел: %v", st.Sources["web"].Values)
	}
}
