package wac

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// File — вложение к отправке. Photo — картинкой в ленте (иначе файлом).
type File struct {
	Path  string
	Name  string
	Photo bool
}

// quote — цитата сообщения для ответа.
func (c *Client) quote(reply *Message) *waE2E.ContextInfo {
	if reply == nil {
		return nil
	}
	ci := &waE2E.ContextInfo{StanzaID: proto.String(reply.WAID), Participant: proto.String(reply.Sender)}
	if reply.FromMe {
		ci.Participant = proto.String(c.Self().String())
	}
	if len(reply.Raw) > 0 {
		var q waE2E.Message
		if proto.Unmarshal(reply.Raw, &q) == nil {
			ci.QuotedMessage = &q
		}
	}
	if ci.QuotedMessage == nil {
		ci.QuotedMessage = &waE2E.Message{Conversation: proto.String(reply.Text)}
	}
	return ci
}

// sent — своё отправленное в хранилище (WhatsApp его обратно не присылает).
func (c *Client) sent(ctx context.Context, chat Chat, resp whatsmeow.SendResponse, msg *waE2E.Message, reply *Message) (*Message, error) {
	content := Parse(msg)
	raw, _ := proto.Marshal(msg)
	m := &Message{Chat: chat.ID, ChatJID: chat.JID, WAID: resp.ID, Sender: c.Self().String(), FromMe: true,
		Time: resp.Timestamp, Text: content.Text, Kind: content.Kind, Raw: raw}
	if m.Time.IsZero() {
		m.Time = time.Now()
	}
	if reply != nil {
		m.ReplyTo = reply.WAID
	}
	if _, _, err := c.Store.Put(ctx, m); err != nil {
		return m, &SentUnsaved{Err: err}
	}
	return m, nil
}

// SentUnsaved — сообщение ушло, но не записалось в хранилище: повторять
// отправку нельзя (получатель увидел бы его дважды).
type SentUnsaved struct{ Err error }

func (e *SentUnsaved) Error() string {
	return "сообщение ушло, но не записалось в хранилище шлюза: " + e.Err.Error()
}
func (e *SentUnsaved) Unwrap() error { return e.Err }

func parseJID(s string) (types.JID, error) {
	jid, err := types.ParseJID(s)
	if err != nil || jid.IsEmpty() {
		return types.EmptyJID, fmt.Errorf("неверный JID WhatsApp %q", s)
	}
	return jid, nil
}

// SendText — текст в чат (ответом на reply, если он есть).
func (c *Client) SendText(ctx context.Context, chat Chat, text string, reply *Message) (*Message, error) {
	wa, err := c.WA(ctx)
	if err != nil {
		return nil, err
	}
	jid, err := parseJID(chat.JID)
	if err != nil {
		return nil, err
	}
	msg := &waE2E.Message{Conversation: proto.String(text)}
	if ci := c.quote(reply); ci != nil {
		msg = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(text), ContextInfo: ci}}
	}
	resp, err := wa.SendMessage(ctx, jid, msg)
	if err != nil {
		return nil, err
	}
	return c.sent(ctx, chat, resp, msg, reply)
}

// SendFile — файл в чат так, как его показала карточка: Photo — картинкой
// в ленте (jpg/png), остальное — документом; caption — подпись.
func (c *Client) SendFile(ctx context.Context, chat Chat, f File, caption string, reply *Message) (*Message, error) {
	wa, err := c.WA(ctx)
	if err != nil {
		return nil, err
	}
	jid, err := parseJID(chat.JID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, err
	}
	mime := http.DetectContentType(data)
	ext := strings.ToLower(filepath.Ext(f.Name))
	switch ext {
	case ".ogg", ".opus":
		mime = "audio/ogg"
	case ".mp3":
		mime = "audio/mpeg"
	case ".m4a":
		mime = "audio/mp4"
	case ".mp4":
		mime = "video/mp4"
	case ".pdf":
		mime = "application/pdf"
	}
	ci := c.quote(reply)
	var (
		msg  *waE2E.Message
		kind whatsmeow.MediaType
	)
	kind = whatsmeow.MediaDocument
	if f.Photo && (strings.HasPrefix(mime, "image/jpeg") || strings.HasPrefix(mime, "image/png")) {
		kind = whatsmeow.MediaImage
	}
	up, err := wa.Upload(ctx, data, kind)
	if err != nil {
		return nil, fmt.Errorf("WhatsApp не принял файл %s: %w", f.Name, err)
	}
	switch kind {
	case whatsmeow.MediaImage:
		msg = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: optional(caption), Mimetype: proto.String(mime),
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: &up.FileLength, ContextInfo: ci}}
	default:
		msg = &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Caption: optional(caption), Mimetype: proto.String(mime),
			FileName: proto.String(f.Name), Title: proto.String(f.Name),
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: &up.FileLength, ContextInfo: ci}}
	}
	resp, err := wa.SendMessage(ctx, jid, msg)
	if err != nil {
		return nil, err
	}
	return c.sent(ctx, chat, resp, msg, reply)
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return proto.String(s)
}

// Edit — новый текст своего сообщения.
func (c *Client) Edit(ctx context.Context, m *Message, text string) error {
	wa, err := c.WA(ctx)
	if err != nil {
		return err
	}
	jid, err := parseJID(m.ChatJID)
	if err != nil {
		return err
	}
	var content *waE2E.Message
	if m.Kind != "" && len(m.Raw) > 0 { // у вложения правится подпись
		var orig waE2E.Message
		if err := proto.Unmarshal(m.Raw, &orig); err == nil {
			if setCaption(&orig, text) {
				content = &orig
			}
		}
	}
	if content == nil {
		content = &waE2E.Message{Conversation: proto.String(text)}
	}
	if _, err := wa.SendMessage(ctx, jid, wa.BuildEdit(jid, m.WAID, content)); err != nil {
		return err
	}
	raw, _ := proto.Marshal(content)
	c.Store.Edit(ctx, m.Chat, m.WAID, text, raw, time.Now())
	return nil
}

func setCaption(m *waE2E.Message, text string) bool {
	switch {
	case m.ImageMessage != nil:
		m.ImageMessage.Caption = proto.String(text)
	case m.VideoMessage != nil:
		m.VideoMessage.Caption = proto.String(text)
	case m.DocumentMessage != nil:
		m.DocumentMessage.Caption = proto.String(text)
	default:
		return false
	}
	return true
}

// React — реакция владельца на сообщение ("" — снять).
func (c *Client) React(ctx context.Context, m *Message, emoji string) error {
	wa, err := c.WA(ctx)
	if err != nil {
		return err
	}
	chat, err := parseJID(m.ChatJID)
	if err != nil {
		return err
	}
	sender := c.Self()
	if !m.FromMe {
		if sender, err = parseJID(m.Sender); err != nil {
			return err
		}
	}
	if _, err := wa.SendMessage(ctx, chat, wa.BuildReaction(chat, sender, m.WAID, emoji)); err != nil {
		return err
	}
	c.Store.React(ctx, m.Chat, m.WAID, c.Self().String(), emoji, time.Now())
	return nil
}

// Media — вложение сообщения: байты, MIME, имя файла, длительность (с).
type Media struct {
	Data     []byte
	MIME     string
	Name     string
	Seconds  float64
	Size     int64
	Thumb    []byte // превью (картинка у видео, стикера)
	Caption  string
	Kind     string
	Unpacked bool
}

// MediaInfo — что за вложение, без скачивания.
func MediaInfo(m *Message) (*Media, whatsmeow.DownloadableMessage, error) {
	if m.Kind == "" || len(m.Raw) == 0 {
		return nil, nil, ErrNoMedia
	}
	var msg waE2E.Message
	if err := proto.Unmarshal(m.Raw, &msg); err != nil {
		return nil, nil, err
	}
	if d := msg.GetDocumentWithCaptionMessage(); d != nil {
		msg = *d.GetMessage()
	}
	md := &Media{Kind: m.Kind, Caption: m.Text}
	var dl whatsmeow.DownloadableMessage
	switch {
	case msg.ImageMessage != nil:
		e := msg.GetImageMessage()
		dl = e
		md.MIME, md.Size, md.Thumb, md.Name = e.GetMimetype(), int64(e.GetFileLength()), e.GetJPEGThumbnail(), "photo.jpg"
	case msg.PtvMessage != nil:
		e := msg.GetPtvMessage()
		dl = e
		md.MIME, md.Size, md.Thumb, md.Seconds, md.Name = e.GetMimetype(), int64(e.GetFileLength()), e.GetJPEGThumbnail(), float64(e.GetSeconds()), "video_note.mp4"
	case msg.VideoMessage != nil:
		e := msg.GetVideoMessage()
		dl = e
		md.MIME, md.Size, md.Thumb, md.Seconds, md.Name = e.GetMimetype(), int64(e.GetFileLength()), e.GetJPEGThumbnail(), float64(e.GetSeconds()), "video.mp4"
	case msg.AudioMessage != nil:
		e := msg.GetAudioMessage()
		dl = e
		md.MIME, md.Size, md.Seconds, md.Name = e.GetMimetype(), int64(e.GetFileLength()), float64(e.GetSeconds()), "voice.ogg"
		if !e.GetPTT() {
			md.Name = "audio" + extOf(e.GetMimetype())
		}
	case msg.DocumentMessage != nil:
		e := msg.GetDocumentMessage()
		dl = e
		md.MIME, md.Size, md.Thumb, md.Name = e.GetMimetype(), int64(e.GetFileLength()), e.GetJPEGThumbnail(), e.GetFileName()
		if md.Name == "" {
			md.Name = "file" + extOf(e.GetMimetype())
		}
	case msg.StickerMessage != nil:
		e := msg.GetStickerMessage()
		dl = e
		md.MIME, md.Size, md.Name = e.GetMimetype(), int64(e.GetFileLength()), "sticker.webp"
	default:
		return nil, nil, ErrNoMedia
	}
	return md, dl, nil
}

// ErrNoMedia — у сообщения нет файла, который можно скачать.
var ErrNoMedia = errors.New("в сообщении нет файла")

func extOf(mime string) string {
	switch {
	case strings.Contains(mime, "ogg"):
		return ".ogg"
	case strings.Contains(mime, "mpeg"):
		return ".mp3"
	case strings.Contains(mime, "mp4"):
		return ".m4a"
	case strings.Contains(mime, "pdf"):
		return ".pdf"
	}
	return ""
}

// Download — файл вложения. Старые файлы WhatsApp с серверов убирает; тогда
// ошибка (повторная выгрузка с телефона — не в этой версии).
func (c *Client) Download(ctx context.Context, m *Message, limit int64) (*Media, error) {
	md, dl, err := MediaInfo(m)
	if err != nil {
		return nil, err
	}
	if limit > 0 && md.Size > limit {
		return nil, fmt.Errorf("файл %d МБ больше предела скачивания (%d МБ)", md.Size>>20, limit>>20)
	}
	wa, err := c.WA(ctx)
	if err != nil {
		return nil, err
	}
	data, err := wa.Download(ctx, dl)
	if err != nil {
		if errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) || errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410) {
			return nil, fmt.Errorf("WhatsApp уже убрал этот файл со своих серверов (он есть только на телефоне владельца)")
		}
		return nil, err
	}
	md.Data, md.Size = data, int64(len(data))
	return md, nil
}
