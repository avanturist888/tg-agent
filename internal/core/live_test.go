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
		nil, "живой тест службы", "markdown", nil, false, 0)
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
	d, err := outbox.New(s.DataDir()).Get(id.(string))
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != outbox.Sent || d.MessageID == nil || *d.MessageID <= 0 {
		t.Fatalf("не отправлено или без id: %s %v (%s)", d.Status, d.SendError, res.Summary)
	}
	t.Logf("одобрено за %v, id очереди %d (в Избранном сообщение уйдёт через ~минуту)",
		time.Since(start).Round(time.Millisecond), *d.MessageID)

	// лента: отложенное сообщение реально уйдёт через ~минуту, событие или
	// сверка допишут в ленту его реальный id — он больше прежнего верха
	feed := "saved"
	before := FeedLast(s, feed)
	deadline := time.Now().Add(90 * time.Second)
	for FeedLast(s, feed) <= before {
		if time.Now().After(deadline) {
			t.Fatalf("нового сообщения в ленте нет за 90 с (верх %d)", before)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("в ленте через %v после одобрения (верх %d → %d)",
		time.Since(start).Round(time.Second), before, FeedLast(s, feed))

	// черновик связан с реальным id (scheduled_relinked): правка и уборка
	// проверочных файлов работают по нему
	dl := time.Now().Add(10 * time.Second)
	var d2 *outbox.Draft
	for {
		if d2, err = outbox.New(s.DataDir()).Get(id.(string)); err != nil {
			t.Fatal(err)
		}
		if d2.MessageID != nil && *d2.MessageID != *d.MessageID {
			break
		}
		if time.Now().After(dl) {
			t.Fatalf("черновик не связан с реальным id (остался %d)", *d.MessageID)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("id очереди %d → реальный %d", *d.MessageID, *d2.MessageID)
}
