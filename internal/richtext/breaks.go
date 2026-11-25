package richtext

import (
	"slices"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

var breakParser = goldmark.New(goldmark.WithExtensions(extension.Table)).Parser()

// PreserveBreaks сохраняет одиночные переносы строк (\n) для серверного
// парсера CommonMark Telegram (InputRichMessageMarkdown): добавляет CommonMark
// hard line break ("  ") перед мягкими переносами строк внутри абзацев, чтобы
// Telegram не схлопывал строки и списки с «— » / «– » в одну строку.
// Разбирает текст AST-парсером CommonMark + GFM Table (goldmark), поэтому не
// трогает код (fenced, indented, inline), заголовки (ATX и Setext), таблицы,
// разделители, границы блоков и уже существующие hard breaks.
func PreserveBreaks(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if !strings.Contains(s, "\n") {
		return s
	}

	src := []byte(s)
	doc := breakParser.Parse(text.NewReader(src))

	var positions []int
	_ = ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := node.(type) {
		case *ast.Heading, *ast.CodeBlock, *ast.FencedCodeBlock, *ast.CodeSpan,
			*ast.HTMLBlock, *extast.Table, *extast.TableHeader, *extast.TableRow, *extast.TableCell:
			return ast.WalkSkipChildren, nil
		case *ast.Text:
			if !n.SoftLineBreak() {
				return ast.WalkContinue, nil
			}
			pos := n.Segment.Stop
			if pos < 0 || pos >= len(src) {
				return ast.WalkContinue, nil
			}
			for pos < len(src) && (src[pos] == ' ' || src[pos] == '\t') {
				pos++
			}
			if pos < len(src) && src[pos] == '\n' && !endsWithHardBreak(src[:pos]) {
				positions = append(positions, pos)
			}
		}
		return ast.WalkContinue, nil
	})

	if len(positions) == 0 {
		return s
	}
	slices.Sort(positions)
	positions = slices.Compact(positions)

	var b strings.Builder
	b.Grow(len(s) + 2*len(positions))
	prev := 0
	for _, pos := range positions {
		b.WriteString(s[prev:pos])
		b.WriteString("  ")
		prev = pos
	}
	b.WriteString(s[prev:])
	return b.String()
}

func endsWithHardBreak(beforeNL []byte) bool {
	if len(beforeNL) >= 2 && beforeNL[len(beforeNL)-1] == ' ' && beforeNL[len(beforeNL)-2] == ' ' {
		return true
	}
	bs := 0
	for i := len(beforeNL) - 1; i >= 0 && beforeNL[i] == '\\'; i-- {
		bs++
	}
	return bs%2 == 1
}
