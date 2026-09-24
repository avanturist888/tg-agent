//go:build live

// Живые проверки с настоящим Telegram — только на чате saved (Избранное).
// Нужна запущенная служба: сессию держит она.
// Запуск: go test -tags live -run Live -v ./internal/core
package core

import (
	"context"
	"testing"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/outbox"
	"tgagent/internal/svc"
)

// Черновик → «Отправить» (как из окна управления) → ушло через службу и
// попало в ленту чата.
func TestLiveApproveSendViaService(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if !svc.Up(ctx) {
		t.Skip("служба не запущена")
	}
	s, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Chats["saved"]; !ok {
		t.Skip("нет чата saved")
	}
	out, err := DraftMessage(ctx, s, "saved", "Проверка службы tg-agent: черновик одобрен из окна и **ушёл через службу**.",
		nil, "живой тест службы", "markdown", nil)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := out.Get("draft_id")
	start := time.Now()
	var res struct {
		Summary string `json:"summary"`
	}
	if _, err := svc.Call(ctx, "approve_send", map[string]string{"id": id.(string), "by": "live-test"}, &res); err != nil {
		t.Fatal(err)
	}
	d, err := outbox.New(s.OutboxPath()).Get(id.(string))
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != outbox.Sent || d.MessageID == nil {
		t.Fatalf("не отправлено: %s %v (%s)", d.Status, d.SendError, res.Summary)
	}
	t.Logf("ушло за %v, message_id %d", time.Since(start).Round(time.Millisecond), *d.MessageID)

	// лента: событие или сверка (раз в минуту) должны дописать id
	feed := FeedPath(s, "saved")
	deadline := time.Now().Add(75 * time.Second)
	for LastID(feed) < int(*d.MessageID) {
		if time.Now().After(deadline) {
			t.Fatalf("id %d не попал в ленту за 75 с (последний %d)", *d.MessageID, LastID(feed))
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("в ленте через %v после отправки", time.Since(start).Round(time.Second))
}
