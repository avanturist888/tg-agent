package maxc

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

// SavedChat — «Избранное»: чат с самим собой, id 0.
const SavedChat int64 = 0

// ID — id, который сервер шлёт то числом, то строкой (prevMessageId, ссылки
// пересылки).
type ID int64

func (id *ID) DecodeMsgpack(d *msgpack.Decoder) error {
	v, err := d.DecodeInterfaceLoose()
	if err != nil {
		return err
	}
	switch v := v.(type) {
	case nil:
		*id = 0
	case int64:
		*id = ID(v)
	case uint64:
		*id = ID(v)
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("maxc: id %q: %w", v, err)
		}
		*id = ID(n)
	default:
		return fmt.Errorf("maxc: id типа %T", v)
	}
	return nil
}

// Element — разметка куска текста: STRONG, EMPHASIZED, UNDERLINE,
// STRIKETHROUGH, MONOSPACED, LINK (attributes.url)…
type Element struct {
	Type       string         `json:"type"`
	From       int            `json:"from"`
	Length     int            `json:"length"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// Link — ответ (REPLY) или пересылка (FORWARD); message — исходное.
type Link struct {
	Type      string   `json:"type"`
	MessageID ID       `json:"messageId"`
	ChatID    int64    `json:"chatId"`
	Message   *Message `json:"message"`
}

// Message — сообщение в том виде, в каком его шлёт сервер (поля, нужные
// шлюзу; вложения — как есть).
type Message struct {
	ID         int64            `json:"id"`
	Time       int64            `json:"time"` // мс Unix
	Type       string           `json:"type"` // USER, CHANNEL, CONTROL…
	Sender     int64            `json:"sender"`
	CID        int64            `json:"cid"`
	Text       string           `json:"text"`
	Status     string           `json:"status"` // EDITED, REMOVED или пусто
	UpdateTime int64            `json:"updateTime"`
	Elements   []Element        `json:"elements"`
	Attaches   []map[string]any `json:"attaches"`
	Link       *Link            `json:"link"`
}

// cid — id сообщения со стороны клиента: мс Unix, строго растут (как у PyMax).
var cid struct {
	sync.Mutex
	prev int64
}

func nextCID() int64 {
	cid.Lock()
	defer cid.Unlock()
	cid.prev = max(time.Now().UnixMilli(), cid.prev+1)
	return cid.prev
}

// History — сообщения чата раньше from (мс Unix; 0 — сейчас), не больше
// backward, от старых к новым.
func (c *Conn) History(ctx context.Context, chatID, from int64, backward int) ([]Message, error) {
	if from <= 0 {
		from = time.Now().UnixMilli()
	}
	var r struct {
		Messages []Message `json:"messages"`
	}
	err := c.Invoke(ctx, OpChatHistory, map[string]any{
		"chatId": chatID, "from": from, "forward": 0, "backward": backward,
		"backwardTime": 0, "forwardTime": 0, "getChat": false, "itemType": "REGULAR",
		"getMessages": true, "interactive": false,
	}, &r)
	return r.Messages, err
}

// SendOptions — к тексту сообщения.
type SendOptions struct {
	ReplyTo  int64     // ответ на сообщение
	Elements []Element // разметка
	Silent   bool      // без звука у получателя
}

// Send — отправить текст; вернёт сообщение, как его записал сервер.
func (c *Conn) Send(ctx context.Context, chatID int64, text string, o SendOptions) (*Message, error) {
	msg := map[string]any{"text": text, "cid": nextCID(), "elements": elems(o.Elements), "attaches": []any{}}
	if o.ReplyTo != 0 {
		msg["link"] = map[string]any{"type": "REPLY", "messageId": o.ReplyTo}
	}
	var r struct {
		Message Message `json:"message"`
	}
	if err := c.Invoke(ctx, OpMsgSend, map[string]any{"chatId": chatID, "message": msg, "notify": !o.Silent}, &r); err != nil {
		return nil, err
	}
	if r.Message.ID == 0 {
		return nil, errors.New("maxc: сервер не вернул отправленное сообщение")
	}
	return &r.Message, nil
}

// Edit — заменить текст своего сообщения.
func (c *Conn) Edit(ctx context.Context, chatID, messageID int64, text string, elements []Element) (*Message, error) {
	var r struct {
		Message Message `json:"message"`
	}
	err := c.Invoke(ctx, OpMsgEdit, map[string]any{
		"chatId": chatID, "messageId": messageID, "text": text, "elements": elems(elements), "attachments": []any{},
	}, &r)
	if err != nil {
		return nil, err
	}
	return &r.Message, nil
}

// Delete — удалить сообщения; forMe — только у себя.
func (c *Conn) Delete(ctx context.Context, chatID int64, ids []int64, forMe bool) error {
	return c.Invoke(ctx, OpMsgDelete, map[string]any{"chatId": chatID, "messageIds": ids, "forMe": forMe}, nil)
}

func elems(e []Element) []Element {
	if e == nil {
		return []Element{} // сервер ждёт массив, не nil
	}
	return e
}

// ── события ──────────────────────────────────────────────────────────────

type EventKind int

const (
	EventOther    EventKind = iota
	EventNew                // новое сообщение (и своё, отправленное с другого устройства)
	EventEdit               // правка: Message с новым текстом
	EventDelete             // удаление: ChatID и MessageIDs
	EventRead               // прочтение: Read
	EventPresence           // кто-то в сети или вышел
)

// Event — разобранное событие сервера. Frame — исходный кадр (для
// незнакомых опкодов).
type Event struct {
	Kind       EventKind
	ChatID     int64
	Message    *Message // EventNew, EventEdit (и EventDelete, если сервер прислал)
	MessageIDs []int64  // EventDelete
	Unread     int      // непрочитанных в чате после события
	Read       *ReadMark
	Frame      Frame
}

type ReadMark struct {
	UserID      int64 `json:"userId"`
	Mark        int64 `json:"mark"` // мс Unix: прочитано всё до этого времени
	SetAsUnread bool  `json:"setAsUnread"`
}

// ParseEvent — событие из кадра, который сервер прислал сам.
func ParseEvent(f Frame) (Event, error) {
	ev := Event{Frame: f}
	switch f.Opcode {
	case OpNotifMessage, OpMsgEdit:
		var p struct {
			ChatID  int64   `json:"chatId"`
			Message Message `json:"message"`
			Unread  int     `json:"unread"`
		}
		if err := f.Decode(&p); err != nil {
			return ev, err
		}
		ev.ChatID, ev.Message, ev.Unread = p.ChatID, &p.Message, p.Unread
		switch p.Message.Status {
		case "EDITED":
			ev.Kind = EventEdit
		case "REMOVED":
			ev.Kind, ev.MessageIDs = EventDelete, []int64{p.Message.ID}
		default:
			ev.Kind = EventNew
			if f.Opcode == OpMsgEdit {
				ev.Kind = EventEdit
			}
		}
	case OpNotifMsgDelete:
		var p struct {
			ChatID     int64   `json:"chatId"`
			MessageIDs []int64 `json:"messageIds"`
		}
		if err := f.Decode(&p); err != nil {
			return ev, err
		}
		ev.Kind, ev.ChatID, ev.MessageIDs = EventDelete, p.ChatID, p.MessageIDs
	case OpNotifMark:
		var p struct {
			ChatID int64 `json:"chatId"`
			Unread int   `json:"unread"`
			ReadMark
		}
		if err := f.Decode(&p); err != nil {
			return ev, err
		}
		ev.Kind, ev.ChatID, ev.Unread, ev.Read = EventRead, p.ChatID, p.Unread, &p.ReadMark
	case OpNotifPresence:
		ev.Kind = EventPresence
	}
	return ev, nil
}
