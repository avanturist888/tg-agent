package tgc

import (
	"github.com/gotd/td/tg"

	"tgagent/internal/omap"
)

// forwardedFrom — откуда переслано сообщение (nil — не пересланное).
//
// from у пересланного — тот, кто переслал; автор оригинала — здесь: человек
// или канал (from), только имя (name) — если автор скрыл аккаунт в
// настройках пересылки; date — когда написан оригинал; channel_post и
// post_author — у постов каналов.
func forwardedFrom(m *tg.Message, b *Batch) *omap.Map {
	fh, ok := m.GetFwdFrom()
	if !ok {
		return nil
	}
	out := omap.New()
	if p, ok := fh.GetFromID(); ok {
		var who *omap.Map
		switch v := p.(type) {
		case *tg.PeerUser:
			who = person(v.UserID, b, "user")
		case *tg.PeerChat:
			who = person(v.ChatID, b, "chat")
		case *tg.PeerChannel:
			who = person(v.ChannelID, b, "channel")
		}
		if who == nil {
			who = omap.New().Set("id", MarkedPeerID(p)).Set("name", "?")
		}
		if _, ch := p.(*tg.PeerChannel); ch {
			who.Set("channel", true)
		}
		out.Set("from", who)
	}
	if name, ok := fh.GetFromName(); ok && name != "" {
		out.Set("name", name) // автор скрыл аккаунт — известно только имя
	}
	if fh.Date != 0 {
		out.Set("date", isoTime(fh.Date))
	}
	if post, ok := fh.GetChannelPost(); ok {
		out.Set("channel_post", post)
	}
	if author, ok := fh.GetPostAuthor(); ok && author != "" {
		out.Set("post_author", author)
	}
	if fh.Imported {
		out.Set("imported", true) // перенесено из другого мессенджера
	}
	return out
}
