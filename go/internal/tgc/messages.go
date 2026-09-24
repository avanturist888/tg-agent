package tgc

import (
	"context"
	"math"
	"mime"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"tgagent/internal/audit"
	"tgagent/internal/config"
	"tgagent/internal/omap"
	"tgagent/internal/richtext"
)

// Batch — сообщения вместе с авторами из одного ответа Telegram.
type Batch struct {
	Messages []tg.MessageClass
	Users    map[int64]*tg.User
	Chats    map[int64]tg.ChatClass
}

func (c *Conn) unpack(res tg.MessagesMessagesClass) *Batch {
	b := &Batch{Users: map[int64]*tg.User{}, Chats: map[int64]tg.ChatClass{}}
	var users []tg.UserClass
	var chats []tg.ChatClass
	switch r := res.(type) {
	case *tg.MessagesMessages:
		b.Messages, users, chats = r.Messages, r.Users, r.Chats
	case *tg.MessagesMessagesSlice:
		b.Messages, users, chats = r.Messages, r.Users, r.Chats
	case *tg.MessagesChannelMessages:
		b.Messages, users, chats = r.Messages, r.Users, r.Chats
	}
	c.Peers.Remember(users, chats)
	for _, u := range users {
		if user, ok := u.(*tg.User); ok {
			b.Users[user.ID] = user
		}
	}
	for _, ch := range chats {
		b.Chats[ch.GetID()] = ch
	}
	return b
}

// History — сообщения чата (новые сверху, как отдаёт Telegram).
// beforeID — только старше этого id, afterID — только новее.
func (c *Conn) History(ctx context.Context, t Target, limit, beforeID, afterID int, search string) (*Batch, error) {
	all := &Batch{Users: map[int64]*tg.User{}, Chats: map[int64]tg.ChatClass{}}
	offset := beforeID
	for len(all.Messages) < limit {
		want := min(100, limit-len(all.Messages))
		var (
			res tg.MessagesMessagesClass
			err error
		)
		if search != "" {
			res, err = c.API.MessagesSearch(ctx, &tg.MessagesSearchRequest{
				Peer: t.Input, Q: search, Filter: &tg.InputMessagesFilterEmpty{},
				OffsetID: offset, Limit: want, MinID: afterID,
			})
		} else {
			res, err = c.API.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
				Peer: t.Input, OffsetID: offset, Limit: want, MinID: afterID,
			})
		}
		if err != nil {
			return nil, err
		}
		b := c.unpack(res)
		for k, v := range b.Users {
			all.Users[k] = v
		}
		for k, v := range b.Chats {
			all.Chats[k] = v
		}
		got := 0
		for _, m := range b.Messages {
			if _, empty := m.(*tg.MessageEmpty); empty {
				continue
			}
			if afterID > 0 && m.GetID() <= afterID {
				continue
			}
			all.Messages = append(all.Messages, m)
			got++
			offset = m.GetID()
		}
		if got == 0 || len(b.Messages) < want {
			break
		}
	}
	c.Peers.Save()
	return all, nil
}

// Message — одно сообщение по id (nil — нет такого).
func (c *Conn) Message(ctx context.Context, t Target, id int) (tg.MessageClass, *Batch, error) {
	ids := []tg.InputMessageClass{&tg.InputMessageID{ID: id}}
	var (
		res tg.MessagesMessagesClass
		err error
	)
	if ch, ok := t.Input.(*tg.InputPeerChannel); ok {
		res, err = c.API.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash}, ID: ids,
		})
	} else {
		res, err = c.API.MessagesGetMessages(ctx, ids)
	}
	if err != nil {
		return nil, nil, err
	}
	b := c.unpack(res)
	c.Peers.Save()
	for _, m := range b.Messages {
		if _, empty := m.(*tg.MessageEmpty); empty {
			continue
		}
		if m.GetID() == id {
			return m, b, nil
		}
	}
	return nil, b, nil
}

// ── разбор медиа ──────────────────────────────────────────────────────────

func webpageOf(m *tg.Message) *tg.WebPage {
	if media, ok := m.Media.(*tg.MessageMediaWebPage); ok {
		if wp, ok := media.Webpage.(*tg.WebPage); ok {
			return wp
		}
	}
	return nil
}

// PhotoOf — фото сообщения (как Telethon: и у превью ссылки тоже).
func PhotoOf(m *tg.Message) *tg.Photo {
	if media, ok := m.Media.(*tg.MessageMediaPhoto); ok {
		if p, ok := media.Photo.(*tg.Photo); ok {
			return p
		}
		return nil
	}
	if wp := webpageOf(m); wp != nil {
		if p, ok := wp.Photo.(*tg.Photo); ok {
			return p
		}
	}
	return nil
}

// DocumentOf — документ сообщения (и у превью ссылки тоже).
func DocumentOf(m *tg.Message) *tg.Document {
	if media, ok := m.Media.(*tg.MessageMediaDocument); ok {
		if d, ok := media.Document.(*tg.Document); ok {
			return d
		}
		return nil
	}
	if wp := webpageOf(m); wp != nil {
		if d, ok := wp.Document.(*tg.Document); ok {
			return d
		}
	}
	return nil
}

func audioAttr(d *tg.Document) *tg.DocumentAttributeAudio {
	for _, a := range d.Attributes {
		if v, ok := a.(*tg.DocumentAttributeAudio); ok {
			return v
		}
	}
	return nil
}

func videoAttr(d *tg.Document) *tg.DocumentAttributeVideo {
	for _, a := range d.Attributes {
		if v, ok := a.(*tg.DocumentAttributeVideo); ok {
			return v
		}
	}
	return nil
}

func hasAttr[T any](d *tg.Document) bool {
	for _, a := range d.Attributes {
		if _, ok := a.(T); ok {
			return true
		}
	}
	return false
}

func fileName(d *tg.Document) string {
	for _, a := range d.Attributes {
		if v, ok := a.(*tg.DocumentAttributeFilename); ok {
			return v.FileName
		}
	}
	return ""
}

func IsVoice(m *tg.Message) bool {
	d := DocumentOf(m)
	if d == nil {
		return false
	}
	a := audioAttr(d)
	return a != nil && a.Voice
}

func IsVideoNote(m *tg.Message) bool {
	d := DocumentOf(m)
	if d == nil {
		return false
	}
	v := videoAttr(d)
	return v != nil && v.RoundMessage
}

func isVideo(d *tg.Document) bool   { return d != nil && videoAttr(d) != nil }
func isSticker(d *tg.Document) bool { return d != nil && hasAttr[*tg.DocumentAttributeSticker](d) }
func isGIF(d *tg.Document) bool     { return d != nil && hasAttr[*tg.DocumentAttributeAnimated](d) }
func isAudio(d *tg.Document) bool {
	if d == nil {
		return false
	}
	a := audioAttr(d)
	return a != nil && !a.Voice
}

// MediaLabel — короткая пометка о вложении (как в Python-версии).
func MediaLabel(m *tg.Message) string {
	doc := DocumentOf(m)
	switch {
	case PhotoOf(m) != nil:
		return "photo"
	case IsVoice(m):
		return "voice"
	case IsVideoNote(m):
		return "video_note"
	case isVideo(doc):
		return "video"
	case isAudio(doc):
		return "audio"
	case isSticker(doc):
		return "sticker"
	}
	if _, ok := m.Media.(*tg.MessageMediaPoll); ok {
		return "poll"
	}
	if doc != nil {
		if name := fileName(doc); name != "" {
			return "file:" + name
		}
		return "document"
	}
	if _, ok := m.Media.(*tg.MessageMediaWebPage); ok {
		return "link_preview"
	}
	if m.Media != nil {
		if _, empty := m.Media.(*tg.MessageMediaEmpty); !empty {
			return typeName(m.Media)
		}
	}
	return ""
}

func typeName(v any) string {
	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Name()
}

// FileInfo — что Telethon называл msg.file.
type FileInfo struct {
	Name     string // "" — нет имени
	Size     int64
	MIME     string
	Ext      string
	Duration *float64
}

var extByMIME = map[string]string{
	"audio/ogg": ".oga", "video/mp4": ".mp4", "image/jpeg": ".jpg", "image/png": ".png",
	"image/webp": ".webp", "audio/mpeg": ".mp3", "application/pdf": ".pdf", "image/gif": ".gif",
	"video/quicktime": ".mov", "audio/mp4": ".m4a", "application/x-tgsticker": ".tgs", "video/webm": ".webm",
}

func extFor(name, mimeType string) string {
	if ext := filepath.Ext(name); ext != "" {
		return ext
	}
	if ext, ok := extByMIME[mimeType]; ok {
		return ext
	}
	if exts, _ := mime.ExtensionsByType(mimeType); len(exts) > 0 {
		return exts[0]
	}
	return ""
}

func photoSize(p *tg.Photo) int64 {
	var best int64
	for _, s := range p.Sizes {
		switch v := s.(type) {
		case *tg.PhotoSize:
			best = max(best, int64(v.Size))
		case *tg.PhotoSizeProgressive:
			for _, n := range v.Sizes {
				best = max(best, int64(n))
			}
		case *tg.PhotoCachedSize:
			best = max(best, int64(len(v.Bytes)))
		}
	}
	return best
}

// File — описание файла сообщения; nil — файла нет.
func File(m *tg.Message) *FileInfo {
	if p := PhotoOf(m); p != nil {
		return &FileInfo{Size: photoSize(p), MIME: "image/jpeg", Ext: ".jpg"}
	}
	d := DocumentOf(m)
	if d == nil {
		return nil
	}
	f := &FileInfo{Name: fileName(d), Size: d.Size, MIME: d.MimeType}
	f.Ext = extFor(f.Name, f.MIME)
	if a := audioAttr(d); a != nil {
		v := float64(a.Duration)
		f.Duration = &v
	} else if v := videoAttr(d); v != nil {
		dur := v.Duration
		f.Duration = &dur
	}
	return f
}

// ── сообщение → JSON для агента ─────────────────────────────────────────

func isoTime(unix int) string {
	return audit.FormatTime(time.Unix(int64(unix), 0))
}

func person(id int64, b *Batch, kind string) *omap.Map {
	switch kind {
	case "user":
		if u, ok := b.Users[id]; ok {
			name := strings.TrimSpace(strings.Join(nonEmpty(u.FirstName, u.LastName), " "))
			if name == "" {
				name = "?"
			}
			var username any
			if u.Username != "" {
				username = u.Username
			}
			return omap.New().Set("id", u.ID).Set("name", name).Set("username", username).Set("bot", u.Bot)
		}
	case "chat", "channel":
		if ch, ok := b.Chats[id]; ok {
			title, username := chatTitle(ch)
			var un any
			if username != "" {
				un = username
			}
			if title == "" {
				title = "?"
			}
			return omap.New().Set("id", id).Set("name", title).Set("username", un).Set("bot", false)
		}
	}
	return nil
}

func chatTitle(ch tg.ChatClass) (title, username string) {
	switch v := ch.(type) {
	case *tg.Chat:
		return v.Title, ""
	case *tg.Channel:
		return v.Title, v.Username
	case *tg.ChatForbidden:
		return v.Title, ""
	case *tg.ChannelForbidden:
		return v.Title, ""
	}
	return "", ""
}

func nonEmpty(parts ...string) []string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func senderOf(m tg.MessageClass, peerID tg.PeerClass, fromID tg.PeerClass, out bool, selfID int64) (int64, string) {
	p := fromID
	if p == nil {
		if out && selfID != 0 {
			return selfID, "user"
		}
		p = peerID
	}
	switch v := p.(type) {
	case *tg.PeerUser:
		return v.UserID, "user"
	case *tg.PeerChat:
		return v.ChatID, "chat"
	case *tg.PeerChannel:
		return v.ChannelID, "channel"
	}
	return 0, ""
}

func reactionLabel(r tg.ReactionClass) string {
	switch v := r.(type) {
	case *tg.ReactionEmoji:
		return v.Emoticon
	case *tg.ReactionCustomEmoji:
		return "custom:" + strconv.FormatInt(v.DocumentID, 10)
	case *tg.ReactionPaid:
		return "⭐"
	}
	return typeName(r)
}

// Reactions — реакции сообщения для агента (nil — нет).
func Reactions(m *tg.Message) []*omap.Map {
	rs, ok := m.GetReactions()
	if !ok || len(rs.Results) == 0 {
		return nil
	}
	out := make([]*omap.Map, 0, len(rs.Results))
	for _, r := range rs.Results {
		_, mine := r.GetChosenOrder()
		out = append(out, omap.New().Set("emoji", reactionLabel(r.Reaction)).Set("count", r.Count).Set("mine", mine))
	}
	return out
}

// Serialize — сообщение в том виде, как его видит агент.
func (c *Conn) Serialize(m tg.MessageClass, b *Batch) *omap.Map {
	self := c.Peers.SelfID()
	switch v := m.(type) {
	case *tg.MessageService:
		sid, kind := senderOf(m, v.PeerID, v.FromID, v.Out, self)
		out := omap.New().Set("id", v.ID).Set("date", isoTime(v.Date)).Set("from", c.personOrStub(sid, kind, v.FromID, v.PeerID, b)).Set("text", "")
		if h, ok := v.ReplyTo.(*tg.MessageReplyHeader); ok {
			if id, ok := h.GetReplyToMsgID(); ok {
				out.Set("reply_to", id)
			}
		}
		out.Set("service_action", typeName(v.Action))
		return out
	case *tg.Message:
		sid, kind := senderOf(m, v.PeerID, v.FromID, v.Out, self)
		out := omap.New().Set("id", v.ID).Set("date", isoTime(v.Date)).Set("from", c.personOrStub(sid, kind, v.FromID, v.PeerID, b)).Set("text", v.Message)
		if rich, ok := v.GetRichMessage(); ok && v.Message == "" {
			// у rich-сообщения message пустой — текст собираем из блоков
			out.Set("text", richtext.ToMarkdown(rich))
			out.Set("rich", true)
		}
		if h, ok := v.ReplyTo.(*tg.MessageReplyHeader); ok {
			if id, ok := h.GetReplyToMsgID(); ok {
				out.Set("reply_to", id)
			}
		}
		label := MediaLabel(v)
		if label != "" {
			out.Set("media", label)
		}
		if f := File(v); f != nil && PhotoOf(v) == nil && !isSticker(DocumentOf(v)) {
			var name any
			if f.Name != "" {
				name = f.Name
			}
			out.Set("file", omap.New().Set("name", name).Set("size", f.Size).Set("mime_type", f.MIME))
		}
		if f := File(v); f != nil && f.Duration != nil && *f.Duration != 0 {
			switch label {
			case "voice", "video_note", "video", "audio":
				out.Set("duration", math.Round(*f.Duration*10)/10) // секунды
			}
		}
		if g, ok := v.GetGroupedID(); ok {
			out.Set("grouped_id", g) // одинаковый у всех фото одного альбома
		}
		if r := Reactions(v); r != nil {
			out.Set("reactions", r)
		}
		if e, ok := v.GetEditDate(); ok && e != 0 {
			out.Set("edited", isoTime(e))
		}
		return out
	}
	return omap.New().Set("id", m.GetID())
}

func (c *Conn) personOrStub(id int64, kind string, fromID, peerID tg.PeerClass, b *Batch) *omap.Map {
	if p := person(id, b, kind); p != nil {
		return p
	}
	// удалённый аккаунт или канал без автора
	marked := id
	if fromID != nil {
		marked = MarkedPeerID(fromID)
	} else if peerID != nil && kind != "user" {
		marked = MarkedPeerID(peerID)
	}
	return omap.New().Set("id", marked).Set("name", "?").Set("username", nil).Set("bot", false)
}

// ── состояние чатов и список диалогов ────────────────────────────────────

// ChatStatus — непрочитанные и последнее сообщение по чатам белого списка.
func (c *Conn) ChatStatus(ctx context.Context, rules []config.ChatRule) (map[string]*omap.Map, error) {
	type item struct {
		alias string
		peer  tg.PeerClass
	}
	var wanted []item
	var peers []tg.InputDialogPeerClass
	for _, r := range rules {
		t, err := c.Resolve(ctx, r)
		if err != nil {
			return nil, err
		}
		input := t.Input
		if t.Kind == "self" {
			input = &tg.InputPeerSelf{}
		}
		wanted = append(wanted, item{r.Alias, PeerOfTarget(t)})
		peers = append(peers, &tg.InputDialogPeer{Peer: input})
	}
	status := map[string]*omap.Map{}
	if len(peers) == 0 {
		return status, nil
	}
	res, err := c.API.MessagesGetPeerDialogs(ctx, peers)
	if err != nil {
		return nil, err
	}
	c.Peers.Remember(res.Users, res.Chats)
	msgs := map[string]tg.MessageClass{}
	for _, m := range res.Messages {
		msgs[msgKey(m)] = m
	}
	for _, d := range res.Dialogs {
		dlg, ok := d.(*tg.Dialog)
		if !ok {
			continue
		}
		for _, w := range wanted {
			if peerKey(w.peer) != peerKey(dlg.Peer) {
				continue
			}
			var at any
			preview := ""
			if m, ok := msgs[peerKey(dlg.Peer)+":"+strconv.Itoa(dlg.TopMessage)]; ok {
				at = isoTime(messageDate(m))
				if msg, ok := m.(*tg.Message); ok {
					preview = truncateRunes(msg.Message, 160)
				}
			}
			status[w.alias] = omap.New().Set("unread", dlg.UnreadCount).Set("last_message_at", at).Set("last_message_preview", preview)
		}
	}
	c.Peers.Save()
	return status, nil
}

// TopMessages — id последнего сообщения в каждом из чатов (для лент).
func (c *Conn) TopMessages(ctx context.Context, targets map[string]Target) (map[string]int, error) {
	var peers []tg.InputDialogPeerClass
	keys := map[string]string{}
	for alias, t := range targets {
		peers = append(peers, &tg.InputDialogPeer{Peer: t.Input})
		keys[peerKey(PeerOfTarget(t))] = alias
	}
	if len(peers) == 0 {
		return map[string]int{}, nil
	}
	res, err := c.API.MessagesGetPeerDialogs(ctx, peers)
	if err != nil {
		return nil, err
	}
	top := map[string]int{}
	for _, d := range res.Dialogs {
		if dlg, ok := d.(*tg.Dialog); ok {
			if alias, ok := keys[peerKey(dlg.Peer)]; ok {
				top[alias] = dlg.TopMessage
			}
		}
	}
	return top, nil
}

// DialogRow — строка `tg dialogs`.
type DialogRow struct {
	ID       int64
	Title    string
	Username string
	Kind     string
	Unread   int
	Archived bool
}

// ListDialogs — все диалоги аккаунта; только для человека, агенту не отдаётся.
func (c *Conn) ListDialogs(ctx context.Context, limit int) ([]DialogRow, error) {
	page, err := c.IterDialogs(ctx, limit)
	if err != nil {
		return nil, err
	}
	rows := make([]DialogRow, 0, len(page.Dialogs))
	for _, d := range page.Dialogs {
		row := DialogRow{ID: MarkedPeerID(d.Peer), Unread: d.Unread, Archived: d.FolderID != 0}
		switch p := d.Peer.(type) {
		case *tg.PeerUser:
			row.Kind = "user"
			if u, ok := page.Users[p.UserID]; ok {
				row.Title = strings.TrimSpace(strings.Join(nonEmpty(u.FirstName, u.LastName), " "))
				row.Username = u.Username
			}
		case *tg.PeerChat:
			row.Kind = "group"
			if ch, ok := page.Chats[p.ChatID]; ok {
				row.Title, _ = chatTitle(ch)
			}
		case *tg.PeerChannel:
			row.Kind = "group"
			if ch, ok := page.Chats[p.ChannelID]; ok {
				row.Title, row.Username = chatTitle(ch)
				if channel, ok := ch.(*tg.Channel); ok && channel.Broadcast {
					row.Kind = "channel"
				}
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// RPCMessage — код ошибки Telegram (REACTION_INVALID и т.п.) или "".
func RPCMessage(err error) string {
	if e, ok := tgerr.As(err); ok {
		return e.Type
	}
	return ""
}

// MessageT — обычное сообщение (для проверок типа снаружи пакета).
type MessageT = tg.Message
