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
