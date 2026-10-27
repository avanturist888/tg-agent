package maxc

import (
	"fmt"
	"testing"
)

func TestMarkdown(t *testing.T) {
	for _, tc := range []struct{ in, text, els string }{
		{"просто текст", "просто текст", ""},
		{"**жирный** и *курсив*", "жирный и курсив", "STRONG 0+6, EMPHASIZED 9+6"},
		{"__под__ ~~зач~~ _кур_", "под зач кур", "UNDERLINE 0+3, STRIKETHROUGH 4+3, EMPHASIZED 8+3"},
		{"код `x := 1` тут", "код x := 1 тут", "MONOSPACED 4+6"},
		{"```go\nfmt.Println(\"*\")\n```", "fmt.Println(\"*\")", "CODE 0+16"},
		{"[сайт](https://max.ru) ок", "сайт ок", "LINK 0+4 https://max.ru"},
		{"# Заголовок\nтекст", "Заголовок\nтекст", "STRONG 0+9"},
		{"* пункт\n* ещё", "* пункт\n* ещё", ""},               // список — не курсив
		{"snake_case_name", "snake_case_name", ""},             // внутри слова — не курсив
		{"2 * 3 = 6, **без пары", "2 * 3 = 6, **без пары", ""}, // непарное — текстом
		{"😀 **да**", "😀 да", "STRONG 3+2"},                     // эмодзи — две единицы UTF-16
	} {
		text, els := Markdown(tc.in)
		got := ""
		for i, e := range els {
			if i > 0 {
				got += ", "
			}
			got += fmt.Sprintf("%s %d+%d", e.Type, e.From, e.Length)
			if u, ok := e.Attributes["url"]; ok {
				got += fmt.Sprint(" ", u)
			}
		}
		if text != tc.text || got != tc.els {
			t.Errorf("%q:\n  текст %q, ждали %q\n  элементы %q, ждали %q", tc.in, text, tc.text, got, tc.els)
		}
	}
}
