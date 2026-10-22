package tgc

import (
	"context"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"tgagent/internal/omap"
	"tgagent/internal/richtext"
)

// MessageText — текст сообщения, как его видит агент (у rich — markdown).
func MessageText(m *tg.Message) string {
	if rich, ok := m.GetRichMessage(); ok && m.Message == "" {
		return richtext.ToMarkdown(rich)
	}
	return m.Message
}

// EditMessage — заменить текст своего сообщения. Разметка — как у
// SendMessage: markdown — rich message, не прошёл — классический разбор.
func (c *Conn) EditMessage(ctx context.Context, t Target, id int, text, format string) (*omap.Map, error) {
	edit := func(req *tg.MessagesEditMessageRequest) error {
		req.Peer, req.ID, req.NoWebpage = t.Input, id, true
		_, err := c.API.MessagesEditMessage(ctx, req)
		if tgerr.Is(err, "MESSAGE_NOT_MODIFIED") {
			return nil // тот же текст — правка и так на месте
		}
		return err
	}
	result := func(format string) *omap.Map {
		return omap.New().Set("message_id", id).Set("edited", true).Set("format", format)
	}
	if format == "markdown" {
		var reason string
		rich, err := c.richInput(ctx, text)
		if err == nil {
			req := &tg.MessagesEditMessageRequest{}
			req.SetMessage("")
			req.SetRichMessage(rich)
			err := edit(req)
			if err == nil {
				return result("rich_markdown"), nil
			}
			if _, ok := tgerr.As(err); !ok {
				return nil, err
			}
			reason = RPCErrorText(err)
		} else {
			if ctx.Err() != nil {
				return nil, err
			}
			reason = "упоминания: " + RPCErrorText(err)
		}
		plain, entities := ParseMarkdown(text)
		for i, e := range entities {
			entities[i] = c.mentionEntity(e)
		}
		req2 := &tg.MessagesEditMessageRequest{}
		req2.SetMessage(plain)
		if len(entities) > 0 {
			req2.SetEntities(entities)
		}
		if err := edit(req2); err != nil {
			return nil, err
		}
		return result("markdown_fallback").Set("fallback_reason", reason), nil
	}
	req := &tg.MessagesEditMessageRequest{}
	req.SetMessage(text)
	if err := edit(req); err != nil {
		return nil, err
	}
	return result("plain"), nil
}
