package tgc

// Упоминания людей в сообщениях.
//
// В Telegram «@имя» становится упоминанием, только если это username. Человека
// без username упоминают по id: в клиентах — выбором из списка, в разметке —
// [Имя](tg://user?id=123). Агенту упоминания видны в tg_read_chat
// (mentions), а писать их он может той же ссылкой: служба превращает её в
// упоминание (в rich-разметке это делает сам Telegram, в запасном разборе
// markdown — mentionEntity).

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/gotd/td/tg"

	"tgagent/internal/omap"
)

var userLink = regexp.MustCompile(`^tg://user\?id=(\d+)$`)

// UserLinkID — id из ссылки tg://user?id=… (0 — не она).
func UserLinkID(url string) int64 {
	m := userLink.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return 0
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	return id
}

func utf16Slice(text string, off, n int) string {
	u := utf16.Encode([]rune(text))
	if off < 0 || n <= 0 || off+n > len(u) {
		return ""
	}
	return string(utf16.Decode(u[off : off+n]))
}

// mentions — кого упомянули в сообщении (nil — никого).
func mentions(m *tg.Message, b *Batch) []*omap.Map {
	var out []*omap.Map
	add := func(id int64, text string) {
		row := omap.New()
		if id != 0 {
			row.Set("id", id)
			if p := person(id, b, "user"); p != nil {
				if name, ok := p.Get("name"); ok {
					row.Set("name", name)
				}
				if un, ok := p.Get("username"); ok && un != nil {
					row.Set("username", un)
				}
			}
		}
		out = append(out, row.Set("text", text))
	}
	for _, e := range m.Entities {
		switch v := e.(type) {
		case *tg.MessageEntityMentionName:
			add(v.UserID, utf16Slice(m.Message, v.Offset, v.Length))
		case *tg.MessageEntityMention: // @username
			text := utf16Slice(m.Message, v.Offset, v.Length)
			out = append(out, omap.New().Set("username", strings.TrimPrefix(text, "@")).Set("text", text))
		}
	}
	if rich, ok := m.GetRichMessage(); ok {
		walkRich(reflect.ValueOf(rich), func(n *tg.TextMentionName) {
			add(n.UserID, plainRich(n.Text))
		})
	}
	return out
}

// walkRich — обойти дерево rich-сообщения и найти упоминания по id.
func walkRich(v reflect.Value, fn func(*tg.TextMentionName)) {
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return
		}
		if n, ok := v.Interface().(*tg.TextMentionName); ok {
			fn(n)
			return
		}
		walkRich(v.Elem(), fn)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				walkRich(v.Field(i), fn)
			}
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			walkRich(v.Index(i), fn)
		}
	}
}

func plainRich(t tg.RichTextClass) string {
	switch n := t.(type) {
	case *tg.TextPlain:
		return n.Text
	case *tg.TextConcat:
		var b strings.Builder
		for _, x := range n.Texts {
			b.WriteString(plainRich(x))
		}
		return b.String()
	case nil:
		return ""
	}
	return strings.Trim(fmt.Sprint(t), "{}")
}

// mentionEntity — ссылка tg://user?id=… в запасном разборе markdown:
// упоминание по id (access_hash — из кэша; не знаем — пусть будет ссылкой).
func (c *Conn) mentionEntity(e tg.MessageEntityClass) tg.MessageEntityClass {
	link, ok := e.(*tg.MessageEntityTextURL)
	if !ok {
		return e
	}
	id := UserLinkID(link.URL)
	if id == 0 || c == nil || c.Peers == nil {
		return e
	}
	hash, ok := c.Peers.user(id)
	if !ok {
		return e
	}
	return &tg.InputMessageEntityMentionName{Offset: link.Offset, Length: link.Length,
		UserID: &tg.InputUser{UserID: id, AccessHash: hash}}
}
