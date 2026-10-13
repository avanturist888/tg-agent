package core

import (
	"strings"
	"testing"

	"tgagent/internal/bot"
	"tgagent/internal/outbox"
)

func TestEditOnlyOwnSentMessages(t *testing.T) {
	s := watchEnv(t)
	box := outbox.New(s.OutboxPath())
	d, err := box.Create(outbox.CreateOpts{Chat: "test", Text: "было", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Approve(d.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := box.MarkSent(d.ID, 555, "2026-10-13T10:00:00+00:00"); err != nil {
		t.Fatal(err)
	}
	if got, err := editTarget(s, "test", 555); err != nil || got.ID != d.ID {
		t.Fatalf("своё отправленное: %v", err)
	}
	if _, err := editTarget(s, "test", 556); err == nil || !strings.Contains(err.Error(), "не через tg-agent") {
		t.Fatalf("чужое сообщение: %v", err)
	}
	if _, err := editTarget(s, "other", 555); err == nil {
		t.Fatal("тот же id в другом чате — не наше")
	}

	// правка — тот же черновик, в карточке «было → станет»
	e, err := box.Create(outbox.CreateOpts{Chat: "test", Text: "стало", TTLMin: 60, EditOf: 555, EditOld: "было"})
	if err != nil {
		t.Fatal(err)
	}
	card := bot.CardMarkdown(e, "Тест", "опечатка", "")
	for _, want := range []string{"исправить своё сообщение", "Было:", "> было", "Станет:", "стало"} {
		if !strings.Contains(card, want) {
			t.Fatalf("в карточке нет %q:\n%s", want, card)
		}
	}
	again, _ := box.Get(e.ID)
	if again.EditOf != 555 || again.EditOld != "было" {
		t.Fatalf("правка не сохранилась в очереди: %+v", again)
	}
}
