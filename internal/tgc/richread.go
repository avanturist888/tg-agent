package tgc

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"
)

// completeRich replaces Telegram's partial rich previews with the full message.
// A failed fetch must not turn a prefix into an apparently complete read.
func (c *Conn) completeRich(ctx context.Context, t Target, b *Batch) error {
	for i, raw := range b.Messages {
		m, ok := raw.(*tg.Message)
		if !ok || !belongs(m, t) {
			continue
		}
		rich, ok := m.GetRichMessage()
		if !ok || !rich.Part {
			continue
		}
		res, err := c.API.MessagesGetRichMessage(ctx, &tg.MessagesGetRichMessageRequest{Peer: t.Input, ID: m.ID})
		if err != nil {
			return fmt.Errorf("полный rich-текст сообщения %d не получен: %w", m.ID, err)
		}
		full := c.unpack(res)
		var found *tg.Message
		for _, candidate := range full.Messages {
			v, ok := candidate.(*tg.Message)
			if !ok || v.ID != m.ID || !belongs(v, t) {
				continue
			}
			if r, ok := v.GetRichMessage(); ok && !r.Part {
				found = v
				break
			}
		}
		if found == nil {
			return fmt.Errorf("Telegram не вернул полный rich-текст сообщения %d", m.ID)
		}
		b.Messages[i] = found
		if b.Users == nil {
			b.Users = map[int64]*tg.User{}
		}
		if b.Chats == nil {
			b.Chats = map[int64]tg.ChatClass{}
		}
		for id, user := range full.Users {
			b.Users[id] = user
		}
		for id, chat := range full.Chats {
			b.Chats[id] = chat
		}
	}
	return nil
}
