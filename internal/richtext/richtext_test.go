package richtext

import (
	"testing"

	"github.com/gotd/td/tg"
)

func TestToMarkdown(t *testing.T) {
	rich := tg.RichMessage{Blocks: []tg.PageBlockClass{
		&tg.PageBlockHeading2{Text: &tg.TextPlain{Text: "Итог"}},
		&tg.PageBlockParagraph{Text: &tg.TextConcat{Texts: []tg.RichTextClass{
			&tg.TextPlain{Text: "всё "}, &tg.TextBold{Text: &tg.TextPlain{Text: "готово"}},
		}}},
		&tg.PageBlockList{Items: []tg.PageListItemClass{&tg.PageListItemText{Text: &tg.TextPlain{Text: "раз"}}}},
		&tg.PageBlockDivider{},
	}}
	want := "## Итог\n\nвсё **готово**\n\n- раз\n\n---"
	if got := ToMarkdown(rich); got != want {
		t.Fatalf("\n%q\n%q", got, want)
	}
}

func TestTextMathAndCustomEmoji(t *testing.T) {
	if got := Text(&tg.TextMath{Source: "E = mc^2"}); got != "$E = mc^2$" {
		t.Fatalf("TextMath = %q, want %q", got, "$E = mc^2$")
	}
	if got := Text(&tg.TextMath{Source: ""}); got != "" {
		t.Fatalf("empty TextMath = %q, want empty", got)
	}
	if got := Text(&tg.TextCustomEmoji{DocumentID: 123, Alt: "🔥"}); got != "🔥" {
		t.Fatalf("TextCustomEmoji = %q, want %q", got, "🔥")
	}
}

func TestPageBlockMathAndFallbackSource(t *testing.T) {
	if got := Block(&tg.PageBlockMath{Source: "x^2 + y^2 = z^2"}, ""); got != "$$\nx^2 + y^2 = z^2\n$$" {
		t.Fatalf("PageBlockMath = %q", got)
	}
	if got := Block(&tg.PageBlockMath{Source: "a + b"}, "  "); got != "  $$\n  a + b\n  $$" {
		t.Fatalf("indented PageBlockMath = %q", got)
	}
	if got := Block(&tg.PageBlockMath{Source: ""}, ""); got != "" {
		t.Fatalf("empty PageBlockMath = %q, want empty", got)
	}

	rich := tg.RichMessage{Blocks: []tg.PageBlockClass{
		&tg.PageBlockParagraph{Text: &tg.TextConcat{Texts: []tg.RichTextClass{
			&tg.TextPlain{Text: "Формула "},
			&tg.TextMath{Source: `\alpha + \beta`},
			&tg.TextPlain{Text: " "},
			&tg.TextCustomEmoji{DocumentID: 1, Alt: "✅"},
		}}},
		&tg.PageBlockMath{Source: `E = mc^2`},
	}}
	want := "Формула $\\alpha + \\beta$ ✅\n\n$$\nE = mc^2\n$$"
	if got := ToMarkdown(rich); got != want {
		t.Fatalf("ToMarkdown:\ngot:  %q\nwant: %q", got, want)
	}

	if got := fallbackText(struct{ Source string }{Source: "fallback_src"}); got != "fallback_src" {
		t.Fatalf("fallbackText(Source) = %q, want %q", got, "fallback_src")
	}
}
