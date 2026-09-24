// Package outbox — очередь черновиков data/outbox.json, общая с Python-версией.
//
// Агент кладёт сюда текст, человек подтверждает. Файл общий для всех
// процессов (и Go, и Python), поэтому любые изменения — под локом, а поля,
// которых эта версия не знает, сохраняются при перезаписи.
package outbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"tgagent/internal/audit"
	"tgagent/internal/lock"
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

// LockTimeout — сколько ждать лок очереди (как в Python-версии).
const LockTimeout = 45 * time.Second

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
}

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

	extra *omap.Map // поля от других версий — возвращаем при записи как есть
}

var known = map[string]bool{
	"id": true, "chat": true, "text": true, "reply_to": true, "created_at": true, "expires_at": true,
	"status": true, "origin": true, "approved_at": true, "sent_at": true, "message_id": true,
	"bot_message_id": true, "fmt": true, "files": true, "send_error": true, "note": true,
	"history": true, "send_at": true,
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
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*d = Draft{
		ID: aux.ID, Chat: aux.Chat, Text: aux.Text, ReplyTo: aux.ReplyTo, CreatedAt: aux.CreatedAt,
		ExpiresAt: aux.ExpiresAt, Status: aux.Status, Origin: aux.Origin, ApprovedAt: aux.ApprovedAt,
		SentAt: aux.SentAt, MessageID: aux.MessageID, BotMessageID: aux.BotMessageID, Fmt: aux.Fmt,
		Files: aux.Files, SendError: aux.SendError, Note: aux.Note, History: aux.History, SendAt: aux.SendAt,
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

// Outbox — очередь черновиков.
type Outbox struct{ Path string }

func New(path string) *Outbox {
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	return &Outbox{Path: path}
}

func (o *Outbox) read() ([]*Draft, error) {
	data, err := os.ReadFile(o.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var drafts []*Draft
	if err := json.Unmarshal(data, &drafts); err != nil {
		return nil, fmt.Errorf("outbox.json повреждён: %w", err)
	}
	return drafts, nil
}

func (o *Outbox) write(drafts []*Draft) error {
	if drafts == nil {
		drafts = []*Draft{}
	}
	raw, err := omap.Marshal(drafts)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return err
	}
	tmp := o.Path[:len(o.Path)-len(filepath.Ext(o.Path))] + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	// на Windows замена может упереться в читающего соседа — переждём
	var rerr error
	for i := 0; i < 40; i++ {
		if rerr = os.Rename(tmp, o.Path); rerr == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return rerr
}

func touchExpired(drafts []*Draft) {
	now := time.Now()
	for _, d := range drafts {
		if d.IsExpired(now) {
			d.Status = Expired
		}
	}
}

func (o *Outbox) locked(fn func() error) error {
	return lock.With(context.Background(), o.Path, LockTimeout, fn)
}

// NewID — id вида 0924-0750-85aa (как в Python-версии).
func NewID() string {
	b := make([]byte, 2)
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
}

func (o *Outbox) Create(opts CreateOpts) (*Draft, error) {
	now := time.Now()
	id := opts.ID
	if id == "" {
		id = NewID()
	}
	fmtName := opts.Fmt
	if fmtName == "" {
		fmtName = "markdown"
	}
	d := &Draft{
		ID:        id,
		Chat:      opts.Chat,
		Text:      opts.Text,
		ReplyTo:   opts.ReplyTo,
		CreatedAt: audit.FormatTime(now),
		ExpiresAt: audit.FormatTime(now.Add(time.Duration(opts.TTLMin) * time.Minute)),
		Status:    Pending,
		Origin:    "agent",
		Fmt:       fmtName,
		Files:     opts.Files,
		Note:      opts.Note,
	}
	if opts.SendAt != nil {
		at := audit.FormatTime(*opts.SendAt)
		d.Status = Scheduled
		d.SendAt = &at
		d.AddHistory("event", "scheduled", "by", "auto_send", "at", audit.FormatTime(now), "send_at", at)
	}
	err := o.locked(func() error {
		drafts, err := o.read()
		if err != nil {
			return err
		}
		touchExpired(drafts)
		drafts = append(drafts, d)
		if len(drafts) > 200 {
			drafts = drafts[len(drafts)-200:]
		}
		return o.write(drafts)
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// List — черновики (status == "" — все).
func (o *Outbox) List(status string) ([]*Draft, error) {
	var drafts []*Draft
	err := o.locked(func() error {
		var err error
		drafts, err = o.read()
		if err != nil {
			return err
		}
		touchExpired(drafts)
		return o.write(drafts)
	})
	if err != nil {
		return nil, err
	}
	if status == "" {
		return drafts, nil
	}
	out := drafts[:0:0]
	for _, d := range drafts {
		if d.Status == status {
			out = append(out, d)
		}
	}
	return out, nil
}

func (o *Outbox) Get(id string) (*Draft, error) {
	drafts, err := o.List("")
	if err != nil {
		return nil, err
	}
	for _, d := range drafts {
		if d.ID == id {
			return d, nil
		}
	}
	return nil, &NotFound{ID: id}
}

// Update — изменить черновик под локом; mutate может отказать ошибкой.
func (o *Outbox) Update(id string, mutate func(*Draft) error) (*Draft, error) {
	var found *Draft
	err := o.locked(func() error {
		drafts, err := o.read()
		if err != nil {
			return err
		}
		touchExpired(drafts)
		for _, d := range drafts {
			if d.ID == id {
				if err := mutate(d); err != nil {
					return err
				}
				found = d
				return o.write(drafts)
			}
		}
		return &NotFound{ID: id}
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

func (o *Outbox) Approve(id, by string) (*Draft, error) {
	return o.Update(id, func(d *Draft) error {
		if d.Status != Pending {
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

// ClaimDue — забрать запланированные черновики, чьё время пришло: перевести
// их в approved (by auto_send) под локом, чтобы отправил ровно один процесс.
func (o *Outbox) ClaimDue(now time.Time) ([]*Draft, error) {
	var due []*Draft
	err := o.locked(func() error {
		drafts, err := o.read()
		if err != nil {
			return err
		}
		touchExpired(drafts)
		changed := false
		for _, d := range drafts {
			if d.Status != Scheduled || d.SendAt == nil {
				continue
			}
			at, err := time.Parse(time.RFC3339, *d.SendAt)
			if err != nil || now.Before(at) {
				continue
			}
			ts := audit.FormatTime(now)
			d.Status = Approved
			d.ApprovedAt = &ts
			d.AddHistory("event", "approved", "by", "auto_send", "at", ts)
			due = append(due, d)
			changed = true
		}
		if !changed {
			return nil
		}
		return o.write(drafts)
	})
	return due, err
}

// NextDue — ближайшее время автоотправки (nil — ничего не запланировано).
func (o *Outbox) NextDue() (*time.Time, error) {
	drafts, err := o.List(Scheduled)
	if err != nil {
		return nil, err
	}
	var next *time.Time
	for _, d := range drafts {
		if d.SendAt == nil {
			continue
		}
		at, err := time.Parse(time.RFC3339, *d.SendAt)
		if err != nil {
			continue
		}
		if next == nil || at.Before(*next) {
			next = &at
		}
	}
	return next, nil
}

// ApprovedBy — кем одобрен черновик (последнее событие approved).
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
