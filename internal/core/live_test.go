//go:build live

// Живые проверки с настоящим Telegram — только на чате saved (Избранное).
// Запуск: go test -tags live -run Live -v ./internal/core
package core

import (
	"context"
	"testing"
	"time"

	"tgagent/internal/bot"
	"tgagent/internal/config"
	"tgagent/internal/outbox"
)

// «✅ Отправить сейчас» под карточкой автоотправки: уходит сразу, не ждёт окна.
func TestLiveSendNow(t *testing.T) {
	s, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	rule, ok := s.Chats["saved"]
	if !ok {
		t.Skip("нет чата saved")
	}
	// автоотправку включаем только в памяти — chats.toml не трогаем
	rule.Send, rule.Auto = true, true
	s.Chats["saved"] = rule
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	out, err := DraftMessage(ctx, s, "saved", "Проверка кнопки **«Отправить сейчас»**: ушло сразу, без 30 секунд ожидания.",
		nil, "проверка кнопки «Отправить сейчас»", "markdown", nil)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := out.Get("status"); st != "scheduled" {
		t.Fatalf("статус %v", st)
	}
	id, _ := out.Get("draft_id")
	q := &bot.CallbackQuery{ID: "live-test", Data: "d:" + id.(string) + ":ok"}
	q.From.ID = s.ApprovalChatID
	start := time.Now()
	HandleCallback(ctx, s, q)
	d, err := outbox.New(s.OutboxPath()).Get(id.(string))
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != outbox.Sent || d.MessageID == nil {
		t.Fatalf("не отправлено: %s %v", d.Status, d.SendError)
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Fatalf("слишком долго: %v", took)
	}
	t.Logf("ушло за %v, message_id %d, одобрил %s", time.Since(start).Round(time.Millisecond), *d.MessageID, d.ApprovedBy())
}
