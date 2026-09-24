package tgc

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"mime"
	"path/filepath"
	"strings"

	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"tgagent/internal/omap"
)

func randomID() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return int64(binary.LittleEndian.Uint64(b[:]))
}

// sent — id и дата отправленных сообщений из ответа Telegram, в порядке randomIDs.
func sent(u tg.UpdatesClass, randomIDs []int64) (ids []int, date int) {
	byRandom := map[int64]int{}
	var fallback []int
	collect := func(list []tg.UpdateClass) {
		for _, up := range list {
			switch v := up.(type) {
			case *tg.UpdateMessageID:
				byRandom[v.RandomID] = v.ID
			case *tg.UpdateNewMessage:
				fallback = append(fallback, v.Message.GetID())
				date = max(date, messageDate(v.Message))
			case *tg.UpdateNewChannelMessage:
				fallback = append(fallback, v.Message.GetID())
				date = max(date, messageDate(v.Message))
			}
		}
	}
	switch v := u.(type) {
	case *tg.UpdateShortSentMessage:
		return []int{v.ID}, v.Date
	case *tg.Updates:
		collect(v.Updates)
	case *tg.UpdatesCombined:
		collect(v.Updates)
	case *tg.UpdateShort:
		collect([]tg.UpdateClass{v.Update})
	}
	for _, r := range randomIDs {
		if id, ok := byRandom[r]; ok {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		ids = fallback
	}
	return ids, date
}

func replyTo(id *int64) tg.InputReplyToClass {
	if id == nil || *id == 0 {
		return nil
	}
	return &tg.InputReplyToMessage{ReplyToMsgID: int(*id)}
}

// SendMessage — отправить от аккаунта владельца.
//
// markdown уходит как rich message: заголовки, списки, таблицы, цитаты,
// разделители Telegram отрисовывает сам. Если сервер такое не примет,
// откатываемся на обычное сообщение с классической markdown-разметкой —
// сообщение важнее красоты.
func (c *Conn) SendMessage(ctx context.Context, t Target, text string, reply *int64, format string) (*omap.Map, error) {
	send := func(req *tg.MessagesSendMessageRequest) (int, int, error) {
		req.Peer = t.Input
		req.RandomID = randomID()
		req.NoWebpage = true
		if r := replyTo(reply); r != nil {
			req.ReplyTo = r
		}
		res, err := c.API.MessagesSendMessage(ctx, req)
		if err != nil {
			return 0, 0, err
		}
		ids, date := sent(res, []int64{req.RandomID})
		if len(ids) == 0 {
			return 0, 0, fmt.Errorf("Telegram не вернул id отправленного сообщения")
		}
		return ids[0], date, nil
	}
	result := func(id, date int, format string) *omap.Map {
		var d any
		if date != 0 {
			d = isoTime(date)
		}
		return omap.New().Set("message_id", id).Set("date", d).Set("format", format)
	}

	if format == "markdown" {
		req := &tg.MessagesSendMessageRequest{Message: ""}
		req.SetRichMessage(&tg.InputRichMessageMarkdown{Markdown: text})
		id, date, err := send(req)
		if err == nil {
			return result(id, date, "rich_markdown"), nil
		}
		if _, ok := tgerr.As(err); !ok {
			return nil, err // сеть, отмена — не повод слать второй раз
		}
		reason := RPCErrorText(err)
		plain, entities := ParseMarkdown(text)
		req2 := &tg.MessagesSendMessageRequest{Message: plain}
		if len(entities) > 0 {
			req2.SetEntities(entities)
		}
		id, date, err = send(req2)
		if err != nil {
			return nil, err
		}
		return result(id, date, "markdown_fallback").Set("fallback_reason", reason), nil
	}
	id, date, err := send(&tg.MessagesSendMessageRequest{Message: text})
	if err != nil {
		return nil, err
	}
	return result(id, date, "plain"), nil
}

func mimeOf(path string) string {
	if m := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); m != "" {
		if i := strings.IndexByte(m, ';'); i >= 0 {
			m = m[:i]
		}
		return m
	}
	return "application/octet-stream"
}

// SendFiles — отправить файлы как документы, без пережатия, одним альбомом до 10 штук.
func (c *Conn) SendFiles(ctx context.Context, t Target, paths []string, reply *int64) ([]int, error) {
	up := uploader.NewUploader(c.API)
	var media []tg.InputMediaClass
	for _, p := range paths {
		file, err := up.FromPath(ctx, p)
		if err != nil {
			return nil, err
		}
		media = append(media, &tg.InputMediaUploadedDocument{
			File:       file,
			MimeType:   mimeOf(p),
			Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: filepath.Base(p)}},
			ForceFile:  true,
		})
	}
	if len(media) == 1 {
		req := &tg.MessagesSendMediaRequest{Peer: t.Input, Media: media[0], RandomID: randomID()}
		if r := replyTo(reply); r != nil {
			req.ReplyTo = r
		}
		res, err := c.API.MessagesSendMedia(ctx, req)
		if err != nil {
			return nil, err
		}
		ids, _ := sent(res, []int64{req.RandomID})
		return ids, nil
	}
	// альбом: файлы сначала загружаем как медиа, потом шлём одним запросом
	var single []tg.InputSingleMedia
	var randoms []int64
	for _, m := range media {
		got, err := c.API.MessagesUploadMedia(ctx, &tg.MessagesUploadMediaRequest{Peer: t.Input, Media: m})
		if err != nil {
			return nil, err
		}
		md, ok := got.(*tg.MessageMediaDocument)
		if !ok {
			return nil, fmt.Errorf("Telegram вернул не документ (%s)", typeName(got))
		}
		doc, ok := md.Document.(*tg.Document)
		if !ok {
			return nil, fmt.Errorf("Telegram не вернул документ")
		}
		r := randomID()
		randoms = append(randoms, r)
		single = append(single, tg.InputSingleMedia{
			Media:    &tg.InputMediaDocument{ID: &tg.InputDocument{ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference}},
			RandomID: r,
		})
	}
	req := &tg.MessagesSendMultiMediaRequest{Peer: t.Input, MultiMedia: single}
	if r := replyTo(reply); r != nil {
		req.ReplyTo = r
	}
	res, err := c.API.MessagesSendMultiMedia(ctx, req)
	if err != nil {
		return nil, err
	}
	ids, _ := sent(res, randoms)
	return ids, nil
}

// NormalizeEmoji — Telegram хранит реакции без селектора вариации: «❤», а не
// «❤️». Агенты обычно пишут с ним — без нормализации сервер отвечает
// REACTION_INVALID.
func NormalizeEmoji(e string) string {
	return strings.TrimSpace(strings.ReplaceAll(e, "️", ""))
}

// AvailableReactions — активные стандартные реакции, живой список от Telegram.
func (c *Conn) AvailableReactions(ctx context.Context) ([]string, error) {
	res, err := c.API.MessagesGetAvailableReactions(ctx, 0)
	if err != nil {
		return nil, err
	}
	list, ok := res.(*tg.MessagesAvailableReactions)
	if !ok {
		return nil, nil
	}
	var out []string
	for _, r := range list.Reactions {
		if !r.Inactive {
			out = append(out, r.Reaction)
		}
	}
	return out, nil
}

// SetReaction — поставить реакцию от аккаунта владельца (emoji == "" — снять свою).
func (c *Conn) SetReaction(ctx context.Context, t Target, msgID int, emoji string) ([]*omap.Map, error) {
	var reaction []tg.ReactionClass
	if emoji != "" {
		emoji = NormalizeEmoji(emoji)
		allowed, err := c.AvailableReactions(ctx)
		if err != nil {
			return nil, err
		}
		if len(allowed) > 0 && !contains(allowed, emoji) {
			return nil, &Bad{fmt.Sprintf("«%s» нет среди реакций Telegram. Доступны: %s", emoji, strings.Join(allowed, " "))}
		}
		reaction = []tg.ReactionClass{&tg.ReactionEmoji{Emoticon: emoji}}
	}
	req := &tg.MessagesSendReactionRequest{Peer: t.Input, MsgID: msgID}
	req.SetReaction(reaction)
	if _, err := c.API.MessagesSendReaction(ctx, req); err != nil {
		return nil, err
	}
	m, _, err := c.Message(ctx, t, msgID)
	if err != nil || m == nil {
		return nil, nil
	}
	if msg, ok := m.(*tg.Message); ok {
		return Reactions(msg), nil
	}
	return nil, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
