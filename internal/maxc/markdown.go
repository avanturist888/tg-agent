package maxc

import (
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Markdown — текст без разметки и элементы MAX (как Formatter PyMax):
// **жирный**, *курсив* и _курсив_, __подчёркнутый__, ~~зачёркнутый~~,
// `код`, ```блок кода```, [текст](ссылка), «# заголовок» — жирной строкой.
// Позиции — в единицах UTF-16. Маркер без пары остаётся текстом; «* пункт»
// списка — не курсив (за открывающим маркером не пробел, перед закрывающим —
// тоже).
func Markdown(src string) (string, []Element) {
	var (
		b      strings.Builder
		pos    int // длина b в единицах UTF-16
		els    []Element
		active = map[string]int{}
	)
	emit := func(s string) {
		b.WriteString(s)
		pos += utf16Len(s)
	}
	add := func(typ string, from int, attrs map[string]any) {
		if pos > from {
			els = append(els, Element{Type: typ, From: from, Length: pos - from, Attributes: attrs})
		}
	}
	lineStart := true
	for i := 0; i < len(src); {
		// код — до закрывающего маркера, без разбора внутри
		if code := codeMarker(src[i:]); code != "" {
			if j := strings.Index(src[i+len(code):], code); j >= 0 {
				from := pos
				body := src[i+len(code) : i+len(code)+j]
				if code == "```" { // ```lang\n… — язык не показываем
					if nl := strings.IndexByte(body, '\n'); nl >= 0 && !strings.ContainsAny(body[:nl], " \t") {
						body = body[nl+1:]
					}
					body = strings.TrimSuffix(body, "\n")
				}
				emit(body)
				add(map[string]string{"```": "CODE", "`": "MONOSPACED"}[code], from, nil)
				i += len(code) + j + len(code)
				lineStart = false
				continue
			}
		}
		if lineStart {
			if h := heading(src[i:]); h > 0 {
				end := strings.IndexByte(src[i:], '\n')
				if end < 0 {
					end = len(src) - i
				}
				from := pos
				line, _ := Markdown(src[i+h : i+end]) // разметку внутри заголовка снимаем
				emit(line)
				add("STRONG", from, nil)
				i += end
				continue
			}
		}
		if label, url, n, ok := link(src[i:]); ok {
			from := pos
			emit(label)
			add("LINK", from, map[string]any{"url": url})
			i += n
			lineStart = false
			continue
		}
		if m := marker(src, i); m != "" {
			if from, open := active[m]; open {
				add(markerType[m], from, nil)
				delete(active, m)
				i += len(m)
				continue
			}
			if opens(src, i, m) {
				active[m] = pos
				i += len(m)
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(src[i:])
		emit(src[i : i+size])
		lineStart = r == '\n'
		i += size
	}
	return b.String(), els
}

var markerType = map[string]string{"**": "STRONG", "__": "UNDERLINE", "~~": "STRIKETHROUGH", "*": "EMPHASIZED", "_": "EMPHASIZED"}

func codeMarker(s string) string {
	switch {
	case strings.HasPrefix(s, "```"):
		return "```"
	case strings.HasPrefix(s, "`"):
		return "`"
	}
	return ""
}

func marker(src string, i int) string {
	for _, m := range []string{"**", "__", "~~", "*", "_"} {
		if strings.HasPrefix(src[i:], m) {
			if m == "_" && intraword(src, i) { // snake_case — не курсив
				return ""
			}
			return m
		}
	}
	return ""
}

// opens — маркер открывает выделение: за ним не пробел, и дальше есть
// закрывающий, перед которым тоже не пробел.
func opens(src string, i int, m string) bool {
	rest := src[i+len(m):]
	if r, _ := utf8.DecodeRuneInString(rest); rest == "" || unicode.IsSpace(r) {
		return false
	}
	for off := 0; ; {
		j := strings.Index(rest[off:], m)
		if j < 0 {
			return false
		}
		at := off + j
		before, _ := utf8.DecodeLastRuneInString(rest[:at])
		if at > 0 && !unicode.IsSpace(before) && !(m == "_" && intraword(src, i+len(m)+at)) &&
			!(len(m) == 1 && strings.HasPrefix(rest[at:], m+m)) {
			return true
		}
		off = at + len(m)
	}
}

func intraword(src string, i int) bool {
	if i == 0 || i+1 >= len(src) {
		return false
	}
	before, _ := utf8.DecodeLastRuneInString(src[:i])
	after, _ := utf8.DecodeRuneInString(src[i+1:])
	return isWord(before) && isWord(after)
}

func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// heading — «# », «## »… в начале строки: длина префикса.
func heading(s string) int {
	n := 0
	for n < len(s) && n < 6 && s[n] == '#' {
		n++
	}
	if n == 0 || n >= len(s) || s[n] != ' ' {
		return 0
	}
	return n + 1
}

// link — [текст](ссылка) в начале s.
func link(s string) (label, url string, n int, ok bool) {
	if !strings.HasPrefix(s, "[") {
		return "", "", 0, false
	}
	end := strings.Index(s, "](")
	if end <= 1 || strings.ContainsAny(s[1:end], "\n[") {
		return "", "", 0, false
	}
	close := strings.IndexByte(s[end+2:], ')')
	if close <= 0 {
		return "", "", 0, false
	}
	url = s[end+2 : end+2+close]
	if strings.ContainsAny(url, " \n") {
		return "", "", 0, false
	}
	return s[1:end], url, end + 2 + close + 1, true
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}
