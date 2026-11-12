// Package outbox — очередь черновиков (таблица drafts в data/state.db).
//
// Агент кладёт сюда текст, человек подтверждает. Очередь общая для всех
// процессов профиля, каждое изменение — одна транзакция (db.go). Поля,
// которых этот код не знает (от более новой сборки), сохраняются при
// перезаписи: черновик хранится целиком, как JSON.
package outbox

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"tgagent/internal/audit"
	"tgagent/internal/omap"
)

const (
	Pending   = "pending"
	Approved  = "approved"
	Scheduled = "scheduled" // автоотправка: одобрено сразу, уйдёт в send_at, если не отменят
	Sent      = "sent"
	Cancelled = "cancelled"
	Expired   = "expired"
)

// NotFound — нет черновика с таким id.
type NotFound struct{ ID string }

func (e *NotFound) Error() string {
	return fmt.Sprintf("Черновик '%s' не найден", e.ID)
}

// BadState — действие невозможно в текущем статусе.
type BadState struct{ Msg string }

func (e *BadState) Error() string { return e.Msg }

// FileSnap — снимок вложения черновика.
type FileSnap struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Source string `json:"source,omitempty"`
	Kind   string `json:"kind,omitempty"` // KindPhoto — фото в ленте; пусто — документ без сжатия
}

// PollSpec — опрос вместо текста (tg_draft_poll). Варианты нумеруются с 0.
type PollSpec struct {
	Question  string   `json:"question"`
	Options   []string `json:"options"`
	Anonymous bool     `json:"anonymous"`
	Multiple  bool     `json:"multiple,omitempty"`
	Quiz      bool     `json:"quiz,omitempty"`
	Correct   []int    `json:"correct,omitempty"`
	Solution  string   `json:"solution,omitempty"`
}

// VoteSpec — голос владельца в опросе (tg_vote). Options пусто — снять голос.
// Question и Labels — для карточки: за что именно голос.
type VoteSpec struct {
	MessageID int64    `json:"message_id"`
	Options   []int    `json:"options"`
	Question  string   `json:"question"`
	Labels    []string `json:"labels"`
}

// BotSpec — не сообщение, а действие в BotFather (tg_botfather): служба сама
// ведёт диалог, токен из ответа кладёт в хранилище и удаляет из чата.
type BotSpec struct {
	Action   string `json:"action"`             // newbot, token, revoke, setname, …, purge_tokens
	Bot      string `json:"bot,omitempty"`      // @username бота (кроме newbot и purge_tokens)
	Name     string `json:"name,omitempty"`     // newbot: отображаемое имя
	Username string `json:"username,omitempty"` // newbot: username, кончается на bot
	Value    string `json:"value,omitempty"`    // новое значение для set*
	EnvFile  string `json:"env_file,omitempty"` // куда ещё записать токен (KEY=… в .env)
	EnvKey   string `json:"env_key,omitempty"`
}

// KindPhoto — файл уходит фотографией (Telegram покажет его прямо в ленте).
const KindPhoto = "photo"

// Draft — черновик сообщения.
type Draft struct {
	ID           string
	Chat         string
	Text         string
	ReplyTo      *int64
	CreatedAt    string
	ExpiresAt    string
	Status       string
	Origin       string
	ApprovedAt   *string
	SentAt       *string
	MessageID    *int64
	BotMessageID *int64
	Fmt          string
	Files        []FileSnap
	SendError    *string
	Note         string
	History      []json.RawMessage
	SendAt       *string // только у scheduled
	// Topic — тема форума, куда уйдёт сообщение (0 — не форум), TopicTitle — её название
	Topic      int
	TopicTitle string
	// EditOf — это не новое сообщение, а правка уже отправленного (его id);
	// EditOld — его текст до правки (для карточки «было → станет»)
	EditOf  int64
	EditOld string
	// Poll — черновик опроса (текста и файлов тогда нет)
	Poll *PollSpec
	// Vote — не сообщение, а голос владельца в опросе
	Vote *VoteSpec
	// Bot — не сообщение, а действие в BotFather
	Bot *BotSpec

	extra *omap.Map // поля от других версий — возвращаем при записи как есть
}

var known = map[string]bool{
	"id": true, "chat": true, "text": true, "reply_to": true, "created_at": true, "expires_at": true,
	"status": true, "origin": true, "approved_at": true, "sent_at": true, "message_id": true,
	"bot_message_id": true, "fmt": true, "files": true, "send_error": true, "note": true,
	"history": true, "send_at": true, "topic": true, "topic_title": true, "edit_of": true, "edit_old": true,
	"poll": true, "vote": true, "bot": true,
}

func (d *Draft) MarshalJSON() ([]byte, error) {
	m := omap.New()
	if d.extra != nil {
		m.Merge(d.extra)
	}
	files := d.Files
	if files == nil {
		files = []FileSnap{}
	}
	history := d.History
	if history == nil {
		history = []json.RawMessage{}
	}
	m.Set("id", d.ID).Set("chat", d.Chat).Set("text", d.Text).Set("reply_to", d.ReplyTo).
		Set("created_at", d.CreatedAt).Set("expires_at", d.ExpiresAt).Set("status", d.Status).
		Set("origin", d.Origin).Set("approved_at", d.ApprovedAt).Set("sent_at", d.SentAt).
		Set("message_id", d.MessageID).Set("bot_message_id", d.BotMessageID).Set("fmt", d.Fmt).
		Set("files", files).Set("send_error", d.SendError).Set("note", d.Note).Set("history", history)
	if d.SendAt != nil {
		m.Set("send_at", d.SendAt)
	}
	if d.Topic != 0 {
		m.Set("topic", d.Topic).Set("topic_title", d.TopicTitle)
	}
	if d.EditOf != 0 {
		m.Set("edit_of", d.EditOf).Set("edit_old", d.EditOld)
	}
	if d.Poll != nil {
		m.Set("poll", d.Poll)
	}
	if d.Vote != nil {
		m.Set("vote", d.Vote)
	}
	if d.Bot != nil {
		m.Set("bot", d.Bot)
	}
	return m.MarshalJSON()
}

func (d *Draft) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var aux struct {
		ID           string            `json:"id"`
		Chat         string            `json:"chat"`
		Text         string            `json:"text"`
		ReplyTo      *int64            `json:"reply_to"`
		CreatedAt    string            `json:"created_at"`
		ExpiresAt    string            `json:"expires_at"`
		Status       string            `json:"status"`
		Origin       string            `json:"origin"`
		ApprovedAt   *string           `json:"approved_at"`
		SentAt       *string           `json:"sent_at"`
		MessageID    *int64            `json:"message_id"`
		BotMessageID *int64            `json:"bot_message_id"`
		Fmt          string            `json:"fmt"`
		Files        []FileSnap        `json:"files"`
		SendError    *string           `json:"send_error"`
		Note         string            `json:"note"`
		History      []json.RawMessage `json:"history"`
		SendAt       *string           `json:"send_at"`
		Topic        int               `json:"topic"`
		TopicTitle   string            `json:"topic_title"`
		EditOf       int64             `json:"edit_of"`
		EditOld      string            `json:"edit_old"`
		Poll         *PollSpec         `json:"poll"`
		Vote         *VoteSpec         `json:"vote"`
		Bot          *BotSpec          `json:"bot"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*d = Draft{
		ID: aux.ID, Chat: aux.Chat, Text: aux.Text, ReplyTo: aux.ReplyTo, CreatedAt: aux.CreatedAt,
		ExpiresAt: aux.ExpiresAt, Status: aux.Status, Origin: aux.Origin, ApprovedAt: aux.ApprovedAt,
		SentAt: aux.SentAt, MessageID: aux.MessageID, BotMessageID: aux.BotMessageID, Fmt: aux.Fmt,
		Files: aux.Files, SendError: aux.SendError, Note: aux.Note, History: aux.History, SendAt: aux.SendAt,
		Topic: aux.Topic, TopicTitle: aux.TopicTitle, EditOf: aux.EditOf, EditOld: aux.EditOld, Poll: aux.Poll, Vote: aux.Vote,
		Bot: aux.Bot,
	}
	if d.Status == "" {
		d.Status = Pending
	}
	if d.Origin == "" {
		d.Origin = "agent"
	}
	if d.Fmt == "" {
		d.Fmt = "markdown"
	}
	// незнакомые поля сохраняем в исходном порядке
	dec := json.NewDecoder(bytes.NewReader(data))
	if _, err := dec.Token(); err == nil {
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			key, _ := tok.(string)
			var v json.RawMessage
			if err := dec.Decode(&v); err != nil {
				break
			}
			if !known[key] {
				if d.extra == nil {
					d.extra = omap.New()
				}
				d.extra.Set(key, v)
			}
		}
	}
	_ = raw
	return nil
}

// IsExpired — протух ли ещё не решённый черновик.
func (d *Draft) IsExpired(now time.Time) bool {
	if d.Status != Pending && d.Status != Approved && d.Status != Scheduled {
		return false
	}
	exp, err := time.Parse(time.RFC3339, d.ExpiresAt)
	if err != nil {
		return false
	}
	return now.After(exp)
}

// AddHistory дописывает событие (пары ключ, значение) в историю.
func (d *Draft) AddHistory(kv ...any) {
	m := omap.New()
	for i := 0; i+1 < len(kv); i += 2 {
		k, _ := kv[i].(string)
		m.Set(k, kv[i+1])
	}
	raw, _ := omap.Marshal(m)
	d.History = append(d.History, raw)
}

// HistoryEvents — события истории как map, для проверок.
func (d *Draft) HistoryEvents() []map[string]any {
	out := make([]map[string]any, 0, len(d.History))
	for _, h := range d.History {
		var m map[string]any
		if json.Unmarshal(h, &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// NewID — id вида 0924-0750-85aa1f: дата, время UTC и случайный хвост
// (3 байта: при сотнях черновиков в минуту совпадение почти исключено, а
// Create и FreshID исключают его совсем).
func NewID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("0102-1504") + "-" + hex.EncodeToString(b)
}

// CreateOpts — параметры нового черновика.
type CreateOpts struct {
	ID      string
	Chat    string
	Text    string
	ReplyTo *int64
	TTLMin  int
	Note    string
	Fmt     string
	Files   []FileSnap
	// SendAt != nil — автоотправка: черновик сразу scheduled
	SendAt *time.Time
	// Origin — кто создал: "" — местный агент, "agent:<имя>" — агент из контейнера
	Origin string
	// Topic, TopicTitle — тема форума (0 — не форум)
	Topic      int
	TopicTitle string
	// EditOf, EditOld — правка отправленного сообщения (его id и прежний текст)
	EditOf  int64
	EditOld string
	// Poll — опрос вместо текста
	Poll *PollSpec
	// Vote — голос в опросе вместо сообщения
	Vote *VoteSpec
	// Bot — действие в BotFather вместо сообщения
	Bot *BotSpec
}

// ApprovedBy — кем одобрен черновик (последнее событие approved).
// AgentName — имя агента из контейнера, создавшего черновик ("" — местный агент).
func (d *Draft) AgentName() string {
	name, ok := strings.CutPrefix(d.Origin, "agent:")
	if !ok {
		return ""
	}
	return name
}

func cmpOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (d *Draft) ApprovedBy() string {
	by := ""
	for _, h := range d.HistoryEvents() {
		if h["event"] == "approved" {
			by, _ = h["by"].(string)
		}
	}
	return by
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// EditText — владелец поправил текст ещё не решённого черновика.
func (o *Outbox) EditText(id, text, by string) (*Draft, error) {
	return o.Update(id, func(d *Draft) error {
		if d.Status != Pending && d.Status != Scheduled {
			return &BadState{fmt.Sprintf("Черновик %s в статусе '%s', править поздно", id, d.Status)}
		}
		d.Text = text
		d.AddHistory("event", "edited", "by", by, "at", audit.Now())
		return nil
	})
}
func (o *Outbox) Approve(id, by string) (*Draft, error) {
	return o.Update(id, func(d *Draft) error {
		// scheduled — владелец не стал ждать окна автоотправки: «Отправить сейчас».
		// Одобрение — в транзакции очереди, так что таймер его уже не подхватит.
		if d.Status != Pending && d.Status != Scheduled {
			return &BadState{fmt.Sprintf("Черновик %s в статусе '%s', подтвердить нельзя", id, d.Status)}
		}
		at := audit.Now()
		d.Status = Approved
		d.ApprovedAt = &at
		d.AddHistory("event", "approved", "by", by, "at", at)
		return nil
	})
}

func (o *Outbox) Cancel(id, by string) (*Draft, error) {
	return o.Update(id, func(d *Draft) error {
		if d.Status == Sent || d.Status == Cancelled {
			return &BadState{fmt.Sprintf("Черновик %s уже в статусе '%s'", id, d.Status)}
		}
		if d.Status == Approved && d.ApprovedBy() == "auto_send" {
			// окно отмены закрылось, отправка уже идёт — «отменено» было бы враньём
			return &BadState{fmt.Sprintf("Поздно: окно отмены закрылось, черновик %s уже отправляется.", id)}
		}
		d.Status = Cancelled
		d.AddHistory("event", "cancelled", "by", by, "at", audit.Now())
		return nil
	})
}

func (o *Outbox) AttachCard(id string, messageID int64) (*Draft, error) {
	return o.Update(id, func(d *Draft) error {
		d.BotMessageID = &messageID
		return nil
	})
}

func (o *Outbox) MarkSendError(id, msg string) (*Draft, error) {
	return o.Update(id, func(d *Draft) error {
		short := truncate(msg, 500)
		d.SendError = &short
		d.AddHistory("event", "send_failed", "at", audit.Now(), "error", truncate(msg, 300))
		return nil
	})
}

func (o *Outbox) MarkSent(id string, messageID int64, at string) (*Draft, error) {
	return o.Update(id, func(d *Draft) error {
		d.Status = Sent
		d.SendError = nil
		d.SentAt = &at
		d.MessageID = &messageID
		d.AddHistory("event", "sent", "at", at, "message_id", messageID)
		return nil
	})
}
