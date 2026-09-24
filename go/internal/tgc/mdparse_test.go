package tgc

import (
	"testing"

	"github.com/gotd/td/tg"
)

func TestParseMarkdown(t *testing.T) {
	text, ents := ParseMarkdown("**жир** и __курс__, `код` и [ссылка](https://x.ru) 😀 ~~нет~~")
	if text != "жир и курс, код и ссылка 😀 нет" {
		t.Fatalf("текст: %q", text)
	}
	if len(ents) != 5 {
		t.Fatalf("сущностей %d", len(ents))
	}
	if b := ents[0].(*tg.MessageEntityBold); b.Offset != 0 || b.Length != 3 {
		t.Fatalf("bold %+v", b)
	}
	// эмодзи — две единицы UTF-16: зачёркнутое начинается после него
	s := ents[4].(*tg.MessageEntityStrike)
	if s.Offset != 28 || s.Length != 3 {
		t.Fatalf("strike %+v", s)
	}
	if u := ents[3].(*tg.MessageEntityTextURL); u.URL != "https://x.ru" || u.Length != 6 {
		t.Fatalf("url %+v", u)
	}
}

func TestParseMarkdownUnclosed(t *testing.T) {
	text, ents := ParseMarkdown("2 ** 3 = 8, a_b")
	if text != "2 ** 3 = 8, a_b" || len(ents) != 0 {
		t.Fatalf("%q %v", text, ents)
	}
	text, ents = ParseMarkdown("```go\nx := 1\n```")
	if text != "x := 1" || ents[0].(*tg.MessageEntityPre).Language != "go" {
		t.Fatalf("%q %v", text, ents)
	}
}
