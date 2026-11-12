package core

// Отметка прочитанного — только в каналах: там её никто, кроме владельца,
// не видит. В личках и группах «прочитано» видно собеседникам, а чат без
// непрочитанных владелец может не открыть и пропустить то, что ждёт его
// ответа (принцип 3), — там агент ничего не отмечает.

import (
	"context"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/omap"
	"tgagent/internal/tgc"
)

// MarkRead — tg_mark_read: посты канала до upTo включительно — прочитаны.
func MarkRead(ctx context.Context, s *config.Settings, chat string, upTo int) (*omap.Map, error) {
	rule, err := readable(s, chat)
	if err != nil {
		return nil, err
	}
	if n := netOf(s); n != nil {
		return nil, denied("В %s отмечать прочитанное агент пока не умеет.", n.name())
	}
	if upTo <= 0 {
		return nil, &Bad{Msg: "up_to_id — id последнего поста, который ты обработал (из tg_read_chat)."}
	}
	var unread any
	err = tgc.Run(ctx, s, tgc.Opts{Wait: 30 * time.Second, Caller: "mark_read"}, func(ctx context.Context, c *tgc.Conn) error {
		info, err := c.ChatInfo(ctx, ruleMarkedID(rule, c))
		if err != nil {
			return err
		}
		if info.Kind != "channel" {
			return denied("Отмечать прочитанным можно только посты каналов: в личках и группах «прочитано» " +
				"видят собеседники, а владелец может пропустить то, что ждёт его ответа.")
		}
		t, err := c.Resolve(ctx, rule)
		if err != nil {
			return err
		}
		if err := c.MarkChannelRead(ctx, t, upTo); err != nil {
			return err
		}
		if st, err := c.ChatStatus(ctx, []config.ChatRule{rule}); err == nil && st[rule.Alias] != nil {
			unread, _ = st[rule.Alias].Get("unread")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	logEvent(s, "marked_read", "chat", rule.Alias, "up_to", upTo)
	return omap.New().Set("chat", s.Ref(rule.Alias)).Set("read_up_to", upTo).Set("unread", unread), nil
}
