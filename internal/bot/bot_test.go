package bot

import (
	"strings"
	"testing"

	"tgagent/internal/outbox"
)

func TestCardMarkdownPreservesBreaksForMarkdownDraft(t *testing.T) {
	mdDraft := &outbox.Draft{
		Chat: "test",
		Fmt:  "markdown",
		Text: "Что готово:\n— пункт 1\n— пункт 2",
	}
	card := CardMarkdown(mdDraft, "Тест", "", "")
	wantBody := "Что готово:  \n— пункт 1  \n— пункт 2"
	if !strings.HasSuffix(card, "\n\n"+wantBody) {
		t.Fatalf("CardMarkdown markdown body:\ngot:\n%q\nwant suffix:\n%q", card, wantBody)
	}

	plainDraft := &outbox.Draft{
		Chat: "test",
		Fmt:  "plain",
		Text: "*не жирный*\nвторая строка",
	}
	plainCard := CardMarkdown(plainDraft, "Тест", "", "")
	wantPlain := MDEscape(plainDraft.Text)
	if !strings.HasSuffix(plainCard, "\n\n"+wantPlain) {
		t.Fatalf("CardMarkdown plain body:\ngot:\n%q\nwant suffix:\n%q", plainCard, wantPlain)
	}
}

func TestCardDeleteDraft(t *testing.T) {
	d := &outbox.Draft{
		ID:        "0101-1200-abcdef",
		Chat:      "test",
		Status:    outbox.Pending,
		DeleteOf:  777,
		DeleteOld: "Промежуточный статус <v1>\nвторая строка",
	}
	md := CardMarkdown(d, "Тест", "устарел", "")
	for _, want := range []string{
		"Агент просит удалить своё сообщение",
		"Сообщение #777",
		"> Промежуточный статус \\<v1\\>",
		"> вторая строка",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("CardMarkdown missing %q:\n%s", want, md)
		}
	}
	h := CardHTML(d, "Тест", "устарел", "")
	for _, want := range []string{
		"Агент просит удалить своё сообщение",
		"Сообщение #777",
		"<blockquote>Промежуточный статус &lt;v1&gt;\nвторая строка</blockquote>",
	} {
		if !strings.Contains(h, want) {
			t.Fatalf("CardHTML missing %q:\n%s", want, h)
		}
	}
	if verb(d) != "Удалить" {
		t.Fatalf("verb = %q, want Удалить", verb(d))
	}

	d.Status = outbox.Scheduled
	mdAuto := CardMarkdown(d, "Тест", "", ScheduledStatus(d))
	for _, want := range []string{"Агент удаляет своё сообщение (автоотправка)", "Удалится в"} {
		if !strings.Contains(mdAuto, want) {
			t.Fatalf("CardMarkdown auto_send missing %q:\n%s", want, mdAuto)
		}
	}
}
