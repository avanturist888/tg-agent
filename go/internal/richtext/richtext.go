// Package richtext — rich message (блоки Telegram) → Markdown для агента.
//
// У rich-сообщений поле message пустое, весь текст живёт в блоках. Без этого
// конвертера агент видел бы такие сообщения пустыми.
package richtext

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/gotd/td/tg"
)

func wrap(pattern string, inner string) string {
	if strings.TrimSpace(inner) == "" {
		return inner
	}
	return fmt.Sprintf(pattern, inner)
}

// Text — RichText → строка с markdown-разметкой.
func Text(node tg.RichTextClass) string {
	switch n := node.(type) {
	case nil:
		return ""
	case *tg.TextEmpty:
		return ""
	case *tg.TextPlain:
		return n.Text
	case *tg.TextConcat:
		var b strings.Builder
		for _, t := range n.Texts {
			b.WriteString(Text(t))
		}
		return b.String()
	case *tg.TextBold:
		return wrap("**%s**", Text(n.Text))
	case *tg.TextItalic:
		return wrap("*%s*", Text(n.Text))
	case *tg.TextFixed:
		return wrap("`%s`", Text(n.Text))
	case *tg.TextStrike:
		return wrap("~~%s~~", Text(n.Text))
	case *tg.TextMarked:
		return wrap("==%s==", Text(n.Text))
	case *tg.TextUnderline:
		return Text(n.Text)
	case *tg.TextSubscript:
		return Text(n.Text)
	case *tg.TextSuperscript:
		return Text(n.Text)
	case *tg.TextURL:
		return fmt.Sprintf("[%s](%s)", Text(n.Text), n.URL)
	case *tg.TextEmail:
		return fmt.Sprintf("[%s](mailto:%s)", Text(n.Text), n.Email)
	case *tg.TextPhone:
		return fmt.Sprintf("[%s](tel:%s)", Text(n.Text), n.Phone)
	case *tg.TextImage:
		return ""
	}
	// незнакомый тип из будущих слоёв: берём, что похоже на текст
	return fallbackText(node)
}

func fallbackText(node any) string {
	v := reflect.ValueOf(node)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return ""
	}
	if f := v.FieldByName("Texts"); f.IsValid() && f.Kind() == reflect.Slice {
		var b strings.Builder
		for i := 0; i < f.Len(); i++ {
			if t, ok := f.Index(i).Interface().(tg.RichTextClass); ok {
				b.WriteString(Text(t))
			}
		}
		return b.String()
	}
	if f := v.FieldByName("Text"); f.IsValid() {
		switch t := f.Interface().(type) {
		case tg.RichTextClass:
			return Text(t)
		case string:
			return t
		}
	}
	return ""
}

func items(list []tg.PageListItemClass, indent string) []string {
	var lines []string
	for _, item := range list {
		switch it := item.(type) {
		case *tg.PageListItemBlocks:
			lines = append(lines, nested(it.Blocks, "-", indent)...)
		case *tg.PageListItemText:
			lines = append(lines, indent+"- "+Text(it.Text))
		default:
			lines = append(lines, indent+"- "+fallbackText(item))
		}
	}
	return lines
}

func orderedItems(list []tg.PageListOrderedItemClass, indent string) []string {
	var lines []string
	for n, item := range list {
		num := func(s string) string {
			if s == "" {
				return strconv.Itoa(n+1) + "."
			}
			return s + "."
		}
		switch it := item.(type) {
		case *tg.PageListOrderedItemBlocks:
			lines = append(lines, nested(it.Blocks, num(it.Num), indent)...)
		case *tg.PageListOrderedItemText:
			lines = append(lines, indent+num(it.Num)+" "+Text(it.Text))
		default:
			mark := strconv.Itoa(n+1) + "."
			v := reflect.ValueOf(item).Elem()
			if f := v.FieldByName("Num"); f.IsValid() && f.Kind() == reflect.String && f.String() != "" {
				mark = f.String() + "."
			}
			if f := v.FieldByName("Blocks"); f.IsValid() {
				if bl, ok := f.Interface().([]tg.PageBlockClass); ok {
					lines = append(lines, nested(bl, mark, indent)...)
					continue
				}
			}
			lines = append(lines, indent+mark+" "+fallbackText(item))
		}
	}
	return lines
}

func nested(bl []tg.PageBlockClass, mark, indent string) []string {
	parts := strings.Split(Blocks(bl, indent+"  "), "\n")
	first := strings.TrimSpace(parts[0])
	out := []string{indent + mark + " " + first}
	return append(out, parts[1:]...)
}

func table(b *tg.PageBlockTable) string {
	var rows []string
	for i, row := range b.Rows {
		cells := make([]string, len(row.Cells))
		for j, c := range row.Cells {
			t := ""
			if c.Text != nil {
				t = Text(c.Text)
			}
			cells[j] = strings.ReplaceAll(strings.ReplaceAll(t, "|", `\|`), "\n", " ")
		}
		rows = append(rows, "| "+strings.Join(cells, " | ")+" |")
		if i == 0 {
			rows = append(rows, "|"+strings.Repeat("---|", len(cells)))
		}
	}
	title := Text(b.Title)
	head := ""
	if title != "" {
		head = "**" + title + "**\n"
	}
	return head + strings.Join(rows, "\n")
}

func caption(c tg.PageCaption, kind string) string {
	t := Text(c.Text)
	if t != "" {
		return "[" + kind + ": " + t + "]"
	}
	return "[" + kind + "]"
}

// Block — один блок в markdown.
func Block(b tg.PageBlockClass, indent string) string {
	switch n := b.(type) {
	case *tg.PageBlockHeading1:
		return "# " + Text(n.Text)
	case *tg.PageBlockHeading2:
		return "## " + Text(n.Text)
	case *tg.PageBlockHeading3:
		return "### " + Text(n.Text)
	case *tg.PageBlockHeading4:
		return "#### " + Text(n.Text)
	case *tg.PageBlockHeading5:
		return "##### " + Text(n.Text)
	case *tg.PageBlockHeading6:
		return "###### " + Text(n.Text)
	case *tg.PageBlockTitle:
		return "# " + Text(n.Text)
	case *tg.PageBlockHeader:
		return "# " + Text(n.Text)
	case *tg.PageBlockSubtitle:
		return "## " + Text(n.Text)
	case *tg.PageBlockSubheader:
		return "## " + Text(n.Text)
	case *tg.PageBlockParagraph:
		return indent + Text(n.Text)
	case *tg.PageBlockPreformatted:
		return "```" + n.Language + "\n" + Text(n.Text) + "\n```"
	case *tg.PageBlockBlockquote:
		return quote(Text(n.Text))
	case *tg.PageBlockPullquote:
		return quote(Text(n.Text))
	case *tg.PageBlockDivider:
		return "---"
	case *tg.PageBlockList:
		return strings.Join(items(n.Items, indent), "\n")
	case *tg.PageBlockOrderedList:
		return strings.Join(orderedItems(n.Items, indent), "\n")
	case *tg.PageBlockTable:
		return table(n)
	case *tg.PageBlockDetails:
		return "**" + Text(n.Title) + "**\n" + Blocks(n.Blocks, indent)
	case *tg.PageBlockPhoto:
		return caption(n.Caption, "photo")
	case *tg.PageBlockVideo:
		return caption(n.Caption, "video")
	case *tg.PageBlockAudio:
		return caption(n.Caption, "audio")
	}
	// запасной путь для незнакомых блоков
	v := reflect.ValueOf(b)
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	name := strings.TrimPrefix(v.Type().Name(), "PageBlock")
	if v.Kind() == reflect.Struct {
		if f := v.FieldByName("Blocks"); f.IsValid() {
			if bl, ok := f.Interface().([]tg.PageBlockClass); ok {
				return Blocks(bl, indent)
			}
		}
		if f := v.FieldByName("Text"); f.IsValid() {
			if t, ok := f.Interface().(tg.RichTextClass); ok {
				return indent + Text(t)
			}
		}
	}
	return "[PageBlock" + name + "]"
}

func quote(body string) string {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

// Blocks — последовательность блоков, через пустую строку.
func Blocks(list []tg.PageBlockClass, indent string) string {
	var parts []string
	for _, b := range list {
		if p := Block(b, indent); strings.TrimSpace(p) != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "\n\n")
}

// ToMarkdown — rich message целиком. Лучше пометка, чем падение всего чтения.
func ToMarkdown(rich tg.RichMessage) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = fmt.Sprintf("[rich-сообщение не удалось разобрать: %v]", r)
		}
	}()
	return Blocks(rich.Blocks, "")
}
