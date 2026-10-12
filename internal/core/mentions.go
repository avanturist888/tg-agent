package core

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/omap"
	"tgagent/internal/tgc"
)

// «@Имя» в Telegram — упоминание, только если это username (латиница,
// цифры, _, от 4 символов). «@Светлана» ушла бы простым текстом, и человек
// не получил бы уведомления. Поэтому такое «@Имя» служба сама превращает в
// упоминание по id — [Светлана](tg://user?id=…), — если в недавних
// сообщениях чата ровно один человек с таким именем. Иначе текст остаётся
// как есть, а агенту — предупреждение. Черновик в любом случае создаётся.
//
// «@» считается упоминанием только в начале слова: почта (anna@mail.ru) и
// прочее с @ внутри слова не трогается.

var (
	atToken    = regexp.MustCompile(`(^|[\s(«"'\[])@([\p{L}\p{N}_]+)`)
	usernameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{3,31}$`)
)

// fakeMentions — «@что-то» в начале слова, которое не может быть username.
func fakeMentions(text string) []string {
	var out []string
	for _, m := range atToken.FindAllStringSubmatch(text, -1) {
		if !usernameRe.MatchString(m[2]) && !containsStr(out, m[2]) {
			out = append(out, m[2])
		}
	}
	return out
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// person — автор из недавних сообщений чата.
type person struct {
	ID       int64
	Name     string // имя и фамилия
	First    string
	Username string
}

// matchPerson — кого из авторов агент назвал «@name»: имя, имя с фамилией
// или начало полного имени до пробела (без учёта регистра).
func matchPerson(name string, people []person) []person {
	low := strings.ToLower(name)
	var out []person
	for _, p := range people {
		full := strings.ToLower(p.Name)
		if strings.ToLower(p.First) == low || full == low || strings.HasPrefix(full, low+" ") {
			out = append(out, p)
		}
	}
	return out
}

// replaceMention — «@name» (в начале слова) → ссылка-упоминание.
func replaceMention(text, name, link string) string {
	re := regexp.MustCompile(`(^|[\s(«"'\[])@` + regexp.QuoteMeta(name) + `([^\p{L}\p{N}_]|$)`)
	return re.ReplaceAllString(text, "${1}"+strings.ReplaceAll(link, "$", "$$")+"${2}")
}

// recentPeople — авторы последних сообщений чата.
var recentPeople = func(ctx context.Context, s *config.Settings, rule config.ChatRule) []person {
	var out []person
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_ = tgc.Run(cctx, s, tgc.Opts{}, func(ctx context.Context, c *tgc.Conn) error {
		t, err := c.Resolve(ctx, rule)
		if err != nil {
			return err
		}
		b, err := c.History(ctx, t, 100, 0, 0, "", 0)
		if err != nil {
			return err
		}
		for _, u := range b.Users {
			if u.Self || u.Bot {
				continue
			}
			out = append(out, person{ID: u.ID, Name: strings.TrimSpace(u.FirstName + " " + u.LastName),
				First: u.FirstName, Username: u.Username})
		}
		return nil
	})
	return out
}

// fixMentions — превратить «@Имя» в упоминания, где человек однозначен.
// Вернёт новый текст и заметки для агента (что заменено, что нет и почему).
func fixMentions(ctx context.Context, s *config.Settings, rule config.ChatRule, text, format string) (string, []*omap.Map) {
	names := fakeMentions(text)
	if len(names) == 0 {
		return text, nil
	}
	people := recentPeople(ctx, s, rule)
	var notes []*omap.Map
	for _, n := range names {
		found := matchPerson(n, people)
		note := omap.New().Set("text", "@"+n)
		switch {
		case len(found) == 1 && format == "markdown":
			p := found[0]
			// имя остаётся, как написал агент, — упоминанием по id (тег тот же,
			// что у @username, но текст не меняется)
			link := fmt.Sprintf("[%s](tg://user?id=%d)", n, p.ID)
			text = replaceMention(text, n, link)
			notes = append(notes, note.Set("status", "linked").Set("as", link).Set("person", p.Name))
		case len(found) == 1:
			notes = append(notes, note.Set("status", "plain_text").Set("hint", fmt.Sprintf(
				"format=plain — упоминание по id невозможно: ушло бы простым текстом. Нужен тег — format=markdown и "+
					"[%s](tg://user?id=%d).", n, found[0].ID)))
		case len(found) > 1:
			var opts []string
			for _, p := range found {
				opts = append(opts, fmt.Sprintf("[%s](tg://user?id=%d) — %s", n, p.ID, p.Name))
			}
			notes = append(notes, note.Set("status", "ambiguous").Set("hint",
				"Таких в чате несколько, оставлено простым текстом (без уведомления). Нужен тег — пересоздай "+
					"черновик с одной из ссылок: "+strings.Join(opts, "; ")))
		default:
			notes = append(notes, note.Set("status", "not_found").Set("hint",
				"Это не username, а такого автора в недавних сообщениях нет — уйдёт простым текстом, без "+
					"уведомления. Нужен тег — [Имя](tg://user?id=<from.id из tg_read_chat>)."))
		}
	}
	return text, notes
}
