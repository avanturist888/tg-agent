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
