package tgc

// Диалог с BotFather: служба сама шлёт команды и ждёт ответы.

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/tg"
)

// BotReply — ответ BotFather на шаг диалога.
type BotReply struct {
	Step string // что отправили
	ID   int    // id ответа
	Text string
}

// BotDialog — отправить шаги по одному, после каждого дождаться ответа.
// stop(ответ) == true — дальше не идти (BotFather отказал). Первым вернётся
// id нашего первого сообщения.
func (c *Conn) BotDialog(ctx context.Context, t Target, steps []string, stop func(string) bool) (int, []BotReply, error) {
	var (
		first   int
		replies []BotReply
	)
	for _, step := range steps {
		res, err := c.SendMessage(ctx, t, step, nil, "plain")
		if err != nil {
			return first, replies, err
		}
		v, _ := res.Get("message_id")
		sent, _ := v.(int)
		if first == 0 {
			first = sent
		}
		got, err := c.waitReplies(ctx, t, sent)
		if err != nil {
			return first, replies, err
		}
		if len(got) == 0 {
			return first, replies, fmt.Errorf("BotFather не ответил на «%s» за 30 с", step)
		}
		for _, r := range got {
			r.Step = step
			replies = append(replies, r)
		}
		if stop(got[len(got)-1].Text) {
			break
		}
	}
	return first, replies, nil
}

// waitReplies — входящие после after: ждём первый ответ до 30 с, потом
// ещё немного, если BotFather пишет несколькими сообщениями.
func (c *Conn) waitReplies(ctx context.Context, t Target, after int) ([]BotReply, error) {
	deadline := time.Now().Add(30 * time.Second)
	var out []BotReply
	quietUntil := time.Time{}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(1200 * time.Millisecond):
		}
		last := after
		if len(out) > 0 {
			last = out[len(out)-1].ID
		}
		b, err := c.History(ctx, t, 10, 0, last, "", 0)
		if err != nil {
			return out, err
		}
		fresh := 0
		for i := len(b.Messages) - 1; i >= 0; i-- {
			m, ok := b.Messages[i].(*tg.Message)
			if !ok || m.Out || m.ID <= last {
				continue
			}
			out = append(out, BotReply{ID: m.ID, Text: m.Message})
			fresh++
		}
		switch {
		case fresh > 0:
			quietUntil = time.Now().Add(1500 * time.Millisecond)
		case len(out) > 0 && time.Now().After(quietUntil):
			return out, nil
		}
	}
	return out, nil
}

// DeleteMessages — удалить сообщения лички у обеих сторон.
func (c *Conn) DeleteMessages(ctx context.Context, t Target, ids []int) error {
	if len(ids) == 0 {
		return nil
	}
	if in, ok := t.Input.(*tg.InputPeerChannel); ok {
		_, err := c.API.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: in.ChannelID, AccessHash: in.AccessHash}, ID: ids})
		return err
	}
	_, err := c.API.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{ID: ids, Revoke: true})
	return err
}
