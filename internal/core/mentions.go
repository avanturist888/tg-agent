package core

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/tgc"
)

// «@Имя» в Telegram — упоминание, только если это username (латиница,
// цифры, _, от 4 символов). «@Светлана» уйдёт простым текстом, и человек не
// получит уведомления, — поэтому такой черновик не принимаем, а подсказываем
// упоминание по id: [Светлана](tg://user?id=…).

var (
	atToken    = regexp.MustCompile(`(?:^|[\s(«"'\[])@([\p{L}\p{N}_]+)`)
	usernameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{3,31}$`)
)

// fakeMentions — «@что-то», которое не может быть username.
func fakeMentions(text string) []string {
	var out []string
	for _, m := range atToken.FindAllStringSubmatch(text, -1) {
		if !usernameRe.MatchString(m[1]) && !containsStr(out, m[1]) {
			out = append(out, m[1])
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

// mentionError — отказ с подсказкой: кого из недавних авторов чата агент,
// вероятно, имел в виду, и как упомянуть его по id.
func mentionError(ctx context.Context, s *config.Settings, rule config.ChatRule, names []string) error {
	hints := map[string][]string{}
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
			full := strings.TrimSpace(u.FirstName + " " + u.LastName)
			for _, n := range names {
				if strings.HasPrefix(strings.ToLower(full), strings.ToLower(n)) {
					hint := fmt.Sprintf("[%s](tg://user?id=%d)", n, u.ID)
					if u.Username != "" {
						hint = "@" + u.Username + " или " + hint
					}
					hints[n] = append(hints[n], hint+" — "+full)
				}
			}
		}
		return nil
	})
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "«@%s» — не упоминание: в Telegram @ работает только с username (латиница, от 4 символов), "+
			"иначе это простой текст и человек не получит уведомления. ", n)
		if h := hints[n]; len(h) > 0 {
			fmt.Fprintf(&b, "Упомянуть по id: %s. ", strings.Join(h, "; "))
		} else {
			b.WriteString("Упомянуть человека без username — ссылкой [Имя](tg://user?id=<from.id из tg_read_chat>). ")
		}
	}
	b.WriteString("Или напиши имя без @. Черновик не создан.")
	return &Bad{Msg: b.String()}
}
