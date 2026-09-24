package tgc

import (
	"strings"
	"unicode/utf16"

	"github.com/gotd/td/tg"
)

// ParseMarkdown — классическая markdown-разметка Telegram (как parse_mode="md"
// в клиентах Telegram): **жирный**, __курсив__, ~~зачёркнутый~~, ||спойлер||, `код`,
// ```блок```, [текст](ссылка). Нужна только для запасного пути, когда rich
// message не прошёл.
func ParseMarkdown(src string) (string, []tg.MessageEntityClass) {
	var p mdParser
	p.parse(src)
	return p.out.String(), p.entities
}

type mdParser struct {
	out      strings.Builder
	offset   int // в UTF-16
	entities []tg.MessageEntityClass
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

func (p *mdParser) emit(s string) {
	p.out.WriteString(s)
	p.offset += utf16Len(s)
}

var spans = []struct {
	delim string
	make  func(off, n int) tg.MessageEntityClass
}{
	{"**", func(o, n int) tg.MessageEntityClass { return &tg.MessageEntityBold{Offset: o, Length: n} }},
	{"__", func(o, n int) tg.MessageEntityClass { return &tg.MessageEntityItalic{Offset: o, Length: n} }},
	{"~~", func(o, n int) tg.MessageEntityClass { return &tg.MessageEntityStrike{Offset: o, Length: n} }},
	{"||", func(o, n int) tg.MessageEntityClass { return &tg.MessageEntitySpoiler{Offset: o, Length: n} }},
}

func (p *mdParser) parse(s string) {
	for i := 0; i < len(s); {
		rest := s[i:]
		// блок кода: содержимое как есть
		if strings.HasPrefix(rest, "```") {
			if end := strings.Index(rest[3:], "```"); end >= 0 {
				body := rest[3 : 3+end]
				lang := ""
				if nl := strings.IndexByte(body, '\n'); nl >= 0 && !strings.ContainsAny(body[:nl], " \t") {
					lang, body = body[:nl], body[nl+1:]
				}
				body = strings.TrimSuffix(body, "\n")
				start := p.offset
				p.emit(body)
				p.entities = append(p.entities, &tg.MessageEntityPre{Offset: start, Length: p.offset - start, Language: lang})
				i += 3 + end + 3
				continue
			}
		}
		if rest[0] == '`' {
			if end := strings.IndexByte(rest[1:], '`'); end > 0 {
				start := p.offset
				p.emit(rest[1 : 1+end])
				p.entities = append(p.entities, &tg.MessageEntityCode{Offset: start, Length: p.offset - start})
				i += 1 + end + 1
				continue
			}
		}
		if rest[0] == '[' {
			if closeText := strings.Index(rest, "]("); closeText > 0 {
				if closeURL := strings.IndexByte(rest[closeText+2:], ')'); closeURL > 0 {
					text := rest[1:closeText]
					url := rest[closeText+2 : closeText+2+closeURL]
					if !strings.Contains(text, "\n") && !strings.ContainsAny(url, " \n") {
						start := p.offset
						p.parse(text)
						p.entities = append(p.entities, &tg.MessageEntityTextURL{Offset: start, Length: p.offset - start, URL: url})
						i += closeText + 2 + closeURL + 1
						continue
					}
				}
			}
		}
		matched := false
		for _, sp := range spans {
			if !strings.HasPrefix(rest, sp.delim) {
				continue
			}
			end := strings.Index(rest[len(sp.delim):], sp.delim)
			if end <= 0 {
				continue
			}
			start := p.offset
			p.parse(rest[len(sp.delim) : len(sp.delim)+end])
			if n := p.offset - start; n > 0 {
				p.entities = append(p.entities, sp.make(start, n))
			}
			i += len(sp.delim) + end + len(sp.delim)
			matched = true
			break
		}
		if matched {
			continue
		}
		// обычный символ (целиком, со всеми байтами UTF-8)
		r := []rune(rest)[0]
		p.emit(string(r))
		i += len(string(r))
	}
}
