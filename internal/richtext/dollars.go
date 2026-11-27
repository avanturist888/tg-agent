package richtext

import (
	"slices"
	"strings"
	"unicode"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// EscapeDollars экранирует одиночные денежные и не-математические знаки «$»
// заменой «$» → «\$», чтобы серверный парсер Telegram (InputRichMessageMarkdown)
// не превращал цены и обычный текст в inline-формулы LaTeX (TextMath).
// Код (fenced, indented, inline), HTML-блоки, URL-назначения ссылок,
// уже экранированные «\$», блочную математику «$$» и валидные inline-формулы
// не трогает.
func EscapeDollars(s string) string {
	if !strings.ContainsRune(s, '$') {
		return s
	}

	src := []byte(s)
	doc := breakParser.Parse(text.NewReader(src))

	type dollarPos struct {
		pos    int
		parent ast.Node
	}
	var candidates []dollarPos
	_ = ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := node.(type) {
		case *ast.CodeBlock, *ast.FencedCodeBlock, *ast.CodeSpan, *ast.HTMLBlock:
			return ast.WalkSkipChildren, nil
		case *ast.Text:
			seg := n.Segment
			parent := n.Parent()
			for i := seg.Start; i < seg.Stop; i++ {
				if src[i] != '$' {
					continue
				}
				if isEscaped(src, i) || hasAdjacentDollar(src, i) {
					continue
				}
				candidates = append(candidates, dollarPos{pos: i, parent: parent})
			}
		}
		return ast.WalkContinue, nil
	})

	if len(candidates) == 0 {
		return s
	}
	slices.SortFunc(candidates, func(a, b dollarPos) int { return a.pos - b.pos })
	candidates = slices.CompactFunc(candidates, func(a, b dollarPos) bool { return a.pos == b.pos })

	var toEscape []int
	for i := 0; i < len(candidates); {
		if i+1 < len(candidates) && candidates[i].parent == candidates[i+1].parent &&
			isInlineMath(s, candidates[i].pos, candidates[i+1].pos) {
			i += 2
			continue
		}
		toEscape = append(toEscape, candidates[i].pos)
		i++
	}

	if len(toEscape) == 0 {
		return s
	}

	var b strings.Builder
	b.Grow(len(s) + len(toEscape))
	prev := 0
	for _, pos := range toEscape {
		b.WriteString(s[prev:pos])
		b.WriteByte('\\')
		prev = pos
	}
	b.WriteString(s[prev:])
	return b.String()
}

func isEscaped(src []byte, pos int) bool {
	bs := 0
	for i := pos - 1; i >= 0 && src[i] == '\\'; i-- {
		bs++
	}
	return bs%2 == 1
}

func hasAdjacentDollar(src []byte, pos int) bool {
	return (pos > 0 && src[pos-1] == '$') || (pos+1 < len(src) && src[pos+1] == '$')
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func isASCIIDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func isInlineMath(s string, open, close int) bool {
	if open+1 >= close {
		return false
	}
	if isSpaceByte(s[open+1]) || isSpaceByte(s[close-1]) {
		return false
	}
	if close+1 < len(s) && isASCIIDigit(s[close+1]) {
		return false
	}
	inner := s[open+1 : close]
	if strings.ContainsAny(inner, "\n\r") || strings.Contains(inner, "**") ||
		strings.Contains(inner, "__") || strings.Contains(inner, "](") {
		return false
	}
	if !strings.HasPrefix(inner, `\`) {
		for _, r := range inner {
			if unicode.Is(unicode.Cyrillic, r) {
				return false
			}
		}
	}
	if isASCIIDigit(s[open+1]) && !strings.ContainsAny(inner, `\^_=+<>/-`) {
		return false
	}
	return true
}
