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
	old, oldSend := config.Root, t3SendThread
	config.Root = t.TempDir()
	t.Cleanup(func() { config.Root, t3SendThread = old, oldSend })
	type call struct{ tokenPath, thread, text string }
	var calls []call
	fail := map[string]bool{}
	sendErr := errors.New("тред закрыт")
	t3SendThread = func(_ context.Context, tokenPath, thread, text string) error {
		calls = append(calls, call{tokenPath: tokenPath, thread: thread, text: text})
		if fail[thread] {
			return sendErr
		}
		return nil
	}
	s := &config.Settings{}

	// Без назначения не создаём ни тред, ни сообщение другому агенту.
	if err := wakeProjectAgent(context.Background(), s, "Заголовок", "задание"); err == nil {
		t.Fatal("нет ошибки при отсутствующем назначении")
	}
	if len(calls) != 0 {
		t.Fatalf("отправка без назначения: %+v", calls)
	}

	// назначенный — туда, с заголовком в тексте
	if err := SetUpstreamThread("mcp:abc"); err != nil {
		t.Fatal(err)
	}
	calls = nil
	if err := wakeProjectAgent(context.Background(), s, "Заголовок", "задание"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].tokenPath != t3TokenPath(s) || calls[0].thread != "mcp:abc" ||
		!strings.HasPrefix(calls[0].text, "tg-agent: служебное уведомление") ||
		!strings.Contains(calls[0].text, "«Заголовок»") {
		t.Fatalf("назначенный тред: %+v", calls)
	}

	// Не дошло — ошибка остаётся видимой, никакого нового треда.
	fail["mcp:abc"] = true
	calls = nil
	if err := wakeProjectAgent(context.Background(), s, "Заголовок", "задание"); !errors.Is(err, sendErr) {
		t.Fatalf("ошибка доставки потеряна: %v", err)
	}
	if len(calls) != 1 || calls[0].thread != "mcp:abc" {
		t.Fatalf("адресат после сбоя: %+v", calls)
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
		t.Fatalf("повтор: %v, %+v", err, calls)
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
	// Неудачное закрепление сохраняется независимо от нового состояния источника.
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
	if st, _ := CheckUpstream(ctx, s); st.Sources["web"].Values["v"] != "26.12.0" {
		t.Fatalf("источник не продвинулся после сохранения задания: %v", st.Sources["web"].Values)
	}
	if m, err := upstreamNotices(s); err != nil || !m["web"].CardPending || !m["web"].TaskPending {
		t.Fatalf("не сохранены независимые повторы: %+v %v", m["web"], err)
	}
}
