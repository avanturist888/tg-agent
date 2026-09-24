// Package core — проверки прав и единая логика для CLI, MCP и GUI:
// белый список, политика отправки, черновики, кнопки в боте, ленты.
//
// Любое ослабление проверок здесь — только по прямой просьбе владельца.
package core

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"tgagent/internal/attach"
	"tgagent/internal/audit"
	"tgagent/internal/bot"
	"tgagent/internal/config"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/tgc"
)

// Denied — запрет: чат вне списка, нет прав, режим не тот.
type Denied struct{ Msg string }

func (e *Denied) Error() string { return e.Msg }

func denied(format string, a ...any) error { return &Denied{fmt.Sprintf(format, a...)} }

func clamp(limit int, s *config.Settings) int {
	if limit <= 0 {
		limit = 50
	}
	return max(1, min(limit, s.MaxLimit))
}

func readable(s *config.Settings, chat string) (config.ChatRule, error) {
	rule, err := s.Resolve(chat)
	if err != nil {
		return rule, err
	}
	if !rule.Read {
		return rule, denied("Чат '%s' есть в списке, но чтение для него выключено (read = false).", rule.Alias)
	}
	return rule, nil
}

func sendable(s *config.Settings, chat string) (config.ChatRule, error) {
	rule, err := s.Resolve(chat)
	if err != nil {
		return rule, err
	}
	if s.SendPolicy == "disabled" {
		return rule, denied("Отправка сообщений отключена глобально (TG_SEND_POLICY=disabled).")
	}
	if !rule.Send {
		return rule, denied("В чат '%s' писать запрещено (send = false в config/chats.toml).", rule.Alias)
	}
	return rule, nil
}

// ListChats — tg_list_chats.
func ListChats(ctx context.Context, s *config.Settings, withStatus bool) (*omap.Map, error) {
	var chats []*omap.Map
	for _, rule := range s.Rules() {
		item := rule.AsMap()
		if rule.Read {
			path := FeedPath(s, rule.Alias)
			item.Set("feed", omap.New().Set("path", path).Set("last_id", LastID(path)))
		}
		chats = append(chats, item)
	}
	if withStatus {
		var status map[string]*omap.Map
		err := tgc.Run(ctx, s, tgc.Opts{}, func(ctx context.Context, c *tgc.Conn) error {
			var err error
			status, err = c.ChatStatus(ctx, s.Rules())
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, item := range chats {
			alias, _ := item.Get("alias")
			item.Merge(status[alias.(string)])
		}
	}
	if chats == nil {
		chats = []*omap.Map{}
	}
	return omap.New().Set("send_policy", s.SendPolicy).Set("chats", chats), nil
}

// ReadOpts — что читать.
type ReadOpts struct {
	Limit    int
	BeforeID int
	AfterID  int
	Search   string
	// IDs — конкретные сообщения по номерам (остальные параметры не нужны)
	IDs        []int
	Transcribe bool
}

// ReadChat — tg_read_chat / tg_search_chat.
//
// Ответ на сообщение, которого нет в выдаче (ответили на старое), приходит
// вместе с ним: поле reply_to_message — само исходное сообщение, у голосового
// с расшифровкой. Иначе агент видит только номер и не понимает, о чём речь.
func ReadChat(ctx context.Context, s *config.Settings, chat string, o ReadOpts) (*omap.Map, error) {
	rule, err := readable(s, chat)
	if err != nil {
		return nil, err
	}
	limit := clamp(o.Limit, s)
	if len(o.IDs) > s.MaxLimit {
		return nil, &Bad{Msg: fmt.Sprintf("Не больше %d сообщений за раз.", s.MaxLimit)}
	}
	var messages []*omap.Map
	err = tgc.Run(ctx, s, tgc.Opts{}, func(ctx context.Context, c *tgc.Conn) error {
		t, err := c.Resolve(ctx, rule)
		if err != nil {
			return err
		}
		var batch *tgc.Batch
		if len(o.IDs) > 0 {
			ids := append([]int(nil), o.IDs...)
			sort.Ints(ids)
			if batch, err = c.Messages(ctx, t, ids); err != nil {
				return err
			}
			for _, m := range batch.Messages { // уже по возрастанию
				messages = append(messages, c.Serialize(m, batch))
			}
		} else {
			if batch, err = c.History(ctx, t, limit, o.BeforeID, o.AfterID, o.Search); err != nil {
				return err
			}
			// хронологический порядок: старые сверху
			for i := len(batch.Messages) - 1; i >= 0; i-- {
				messages = append(messages, c.Serialize(batch.Messages[i], batch))
			}
		}
		parents, err := replyParents(ctx, c, t, messages)
		if err != nil {
			return err
		}
		if o.Transcribe {
			cache := tgc.TranscriptCache{Path: s.TranscriptsPath()}
			c.AttachTranscripts(ctx, t, rule, append(messages, parents...), cache, s.TranscribePerCall)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var searchVal any
	if o.Search != "" {
		searchVal = o.Search
	}
	kv := []any{"chat", rule.Alias, "limit", limit, "search", searchVal, "returned", len(messages)}
	if len(o.IDs) > 0 {
		kv = append(kv, "ids", o.IDs)
	}
	_ = audit.Log(s.AuditPath(), "read", kv...)
	var oldest, newest any
	if len(messages) > 0 {
		oldest, _ = messages[0].Get("id")
		newest, _ = messages[len(messages)-1].Get("id")
	}
	if messages == nil {
		messages = []*omap.Map{}
	}
	return omap.New().Set("chat", rule.Alias).Set("title", rule.Title).Set("count", len(messages)).
		Set("oldest_id", oldest).Set("newest_id", newest).Set("messages", messages), nil
}

// replyParents вкладывает в ответы исходные сообщения, которых нет в выдаче,
// — одним запросом на все. Возвращает вложенные (им тоже нужна расшифровка).
func replyParents(ctx context.Context, c *tgc.Conn, t tgc.Target, messages []*omap.Map) ([]*omap.Map, error) {
	have := map[int]bool{}
	for _, m := range messages {
		if id, ok := m.Get("id"); ok {
			have[toInt(id)] = true
		}
	}
	var want []int
	seen := map[int]bool{}
	for _, m := range messages {
		if r, ok := m.Get("reply_to"); ok {
			id := toInt(r)
			if id > 0 && !have[id] && !seen[id] {
				seen[id] = true
				want = append(want, id)
			}
		}
	}
	if len(want) == 0 {
		return nil, nil
	}
	batch, err := c.Messages(ctx, t, want)
	if err != nil {
		return nil, err
	}
	byID := map[int]*omap.Map{}
	var parents []*omap.Map
	for _, m := range batch.Messages {
		p := c.Serialize(m, batch)
		p.Delete("reply_to") // цепочку дальше не разворачиваем: при нужде — ids
		byID[m.GetID()] = p
		parents = append(parents, p)
	}
	for _, m := range messages {
		if r, ok := m.Get("reply_to"); ok {
			if p, ok := byID[toInt(r)]; ok {
				m.Set("reply_to_message", p)
			} else if !have[toInt(r)] {
				m.Set("reply_to_message", nil) // удалено или недоступно
			}
		}
	}
	return parents, nil
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// DownloadFile — вложение из разрешённого чата в data/downloads/<alias>/.
func DownloadFile(ctx context.Context, s *config.Settings, chat string, messageID int) (*omap.Map, error) {
	rule, err := readable(s, chat)
	if err != nil {
		return nil, err
	}
	var got *tgc.Downloaded
	err = tgc.Run(ctx, s, tgc.Opts{}, func(ctx context.Context, c *tgc.Conn) error {
		t, err := c.Resolve(ctx, rule)
		if err != nil {
			return err
		}
		got, err = c.DownloadFile(ctx, t, messageID, filepath.Join(s.DownloadsDir(), rule.Alias), int64(s.MaxDownloadMB)*1024*1024)
		return err
	})
	if err != nil {
		return nil, err
	}
	_ = audit.Log(s.AuditPath(), "download", "chat", rule.Alias, "message_id", messageID, "name", got.Name, "size", got.Size)
	var dur any
	if got.Duration != nil {
		dur = *got.Duration
	}
	return omap.New().Set("chat", rule.Alias).Set("message_id", messageID).Set("path", got.Path).
		Set("name", got.Name).Set("mime_type", got.MIME).Set("size", got.Size).Set("duration", dur).
		Set("cached", got.Cached), nil
}

var reactionHints = map[string]string{
	"REACTION_INVALID":     "в этом чате такая реакция недоступна — попробуй 👍 ❤ 🔥 👏 😁 🎉",
	"REACTION_EMPTY":       "реакция не указана",
	"MESSAGE_ID_INVALID":   "нет такого сообщения в этом чате",
	"MESSAGE_NOT_MODIFIED": "такая реакция уже стоит",
}

// Bad — ошибка во входных данных.
type Bad struct{ Msg string }

func (e *Bad) Error() string { return e.Msg }

// React — реакция сразу, без карточки: эмодзи на чужое сообщение ничего не
// может унести из контекста, а кнопка на каждый 👍 была бы пыткой.
func React(ctx context.Context, s *config.Settings, chat string, messageID int, emoji string) (*omap.Map, error) {
	if !s.Reactions {
		return nil, denied("Реакции выключены владельцем (TG_REACTIONS=off).")
	}
	rule, err := sendable(s, chat)
	if err != nil {
		return nil, err
	}
	emoji = strings.TrimSpace(emoji)
	if len([]rune(emoji)) > 8 {
		return nil, &Bad{"Реакция — это один эмодзи, например 👍."}
	}
	var reactions []*omap.Map
	err = tgc.Run(ctx, s, tgc.Opts{}, func(ctx context.Context, c *tgc.Conn) error {
		t, err := c.Resolve(ctx, rule)
		if err != nil {
			return err
		}
		reactions, err = c.SetReaction(ctx, t, messageID, emoji)
		if code := tgc.RPCMessage(err); code != "" {
			msg := "Telegram отказал: " + err.Error()
			if hint := reactionHints[code]; hint != "" {
				msg += " — " + hint
			}
			return &Bad{msg}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	shown := emoji
	if shown == "" {
		shown = "(снята)"
	}
	_ = audit.Log(s.AuditPath(), "react", "chat", rule.Alias, "message_id", messageID, "emoji", shown)
	var emojiVal, now any
	if emoji != "" {
		emojiVal = emoji
	}
	if reactions != nil {
		now = reactions
	}
	return omap.New().Set("chat", rule.Alias).Set("message_id", messageID).Set("emoji", emojiVal).Set("reactions_now", now), nil
}

// ViewMedia — изображение из разрешённого чата, для показа модели.
func ViewMedia(ctx context.Context, s *config.Settings, chat string, messageID int) (*omap.Map, *tgc.Image, error) {
	rule, err := readable(s, chat)
	if err != nil {
		return nil, nil, err
	}
	var img *tgc.Image
	err = tgc.Run(ctx, s, tgc.Opts{}, func(ctx context.Context, c *tgc.Conn) error {
		t, err := c.Resolve(ctx, rule)
		if err != nil {
			return err
		}
		img, err = c.FetchImage(ctx, t, messageID)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	_ = audit.Log(s.AuditPath(), "view_media", "chat", rule.Alias, "message_id", messageID, "kind", img.Kind)
	meta := omap.New().Set("chat", rule.Alias).Set("message_id", messageID).Set("kind", img.Kind).
		Set("caption", img.Caption).Set("bytes", len(img.Data))
	return meta, img, nil
}

// TranscribeMessage — расшифровать одно голосовое или кружок. Ждёт ПОЛНЫЙ
// текст, но сессию между заходами отпускает: пока Telegram расшифровывает
// длинную запись, остальные (отправка, чтение) не стоят.
func TranscribeMessage(ctx context.Context, s *config.Settings, chat string, messageID int) (*omap.Map, error) {
	rule, err := readable(s, chat)
	if err != nil {
		return nil, err
	}
	cache := tgc.TranscriptCache{Path: s.TranscriptsPath()}
	key := fmt.Sprintf("%s:%d", rule.Alias, messageID)
	if cached, ok := cache.Get(key); ok {
		return omap.New().Set("chat", rule.Alias).Set("message_id", messageID).Set("transcript_status", "done").
			Set("transcript", cached).Set("cached", true), nil
	}
	var (
		duration *float64
		deadline time.Time
		got      = tgc.Transcript{Status: "pending"}
		rpcErr   error
	)
	for {
		err := tgc.Run(ctx, s, tgc.Opts{}, func(ctx context.Context, c *tgc.Conn) error {
			t, err := c.Resolve(ctx, rule)
			if err != nil {
				return err
			}
			if deadline.IsZero() {
				m, _, err := c.Message(ctx, t, messageID)
				if err != nil {
					return err
				}
				msg, ok := m.(*tgc.MessageT)
				if !ok || m == nil || !(tgc.IsVoice(msg) || tgc.IsVideoNote(msg)) {
					return &Bad{fmt.Sprintf("Сообщение %d — не голосовое и не кружок.", messageID)}
				}
				if f := tgc.File(msg); f != nil {
					duration = f.Duration
				}
				deadline = time.Now().Add(tgc.TranscribeTimeout(duration))
			}
			got, err = c.Transcribe(ctx, t, messageID, 8*time.Second)
			if tgc.RPCMessage(err) != "" {
				rpcErr = err
				return nil
			}
			return err
		})
		if err != nil {
			return nil, err
		}
		if rpcErr != nil {
			_ = audit.Log(s.AuditPath(), "transcribe", "chat", rule.Alias, "message_id", messageID, "error", rpcErr.Error())
			return omap.New().Set("chat", rule.Alias).Set("message_id", messageID).Set("duration", durVal(duration)).
				Set("transcript_status", "error").Set("transcript", nil).
				Set("transcript_error", tgc.RPCErrorText(rpcErr)).
				Set("transcript_hint", "Telegram не расшифровал — "+tgc.STTHint), nil
		}
		if got.Status == "done" || !time.Now().Before(deadline) {
			break
		}
		select { // сессия свободна — пусть поработают другие
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(4 * time.Second):
		}
	}
	if got.Status == "done" {
		cache.Put(key, got.Text)
	}
	_ = audit.Log(s.AuditPath(), "transcribe", "chat", rule.Alias, "message_id", messageID, "status", got.Status)
	out := omap.New().Set("chat", rule.Alias).Set("message_id", messageID).Set("duration", durVal(duration)).Set("cached", false)
	tgc.ApplyTranscript(out, got)
	return out, nil
}

func durVal(d *float64) any {
	if d == nil {
		return nil
	}
	return *d
}

// DraftMessage — tg_draft_message. Ничего не отправляет само; в чате с
// автоотправкой ставит сообщение в очередь с окном на отмену.
func DraftMessage(ctx context.Context, s *config.Settings, chat, text string, replyTo *int64, note, format string, files []string) (*omap.Map, error) {
	rule, err := sendable(s, chat)
	if err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)
	if text == "" && len(files) == 0 {
		return nil, &Bad{"Пустое сообщение: нужен текст или файлы."}
	}
	if format == "" {
		format = "markdown"
	}
	if format != "markdown" && format != "plain" {
		return nil, &Bad{"format: только 'markdown' или 'plain'."}
	}
	if n := len([]rune(text)); n > 32768 {
		return nil, &Bad{fmt.Sprintf("Слишком длинно: %d символов, предел Telegram — 32768.", n)}
	}
	id := outbox.NewID()
	var snaps []outbox.FileSnap
	if len(files) > 0 {
		snaps, err = attach.Snapshot(s, id, files)
		if err != nil {
			return nil, err
		}
	}
	opts := outbox.CreateOpts{ID: id, Chat: rule.Alias, Text: text, ReplyTo: replyTo, TTLMin: s.DraftTTLMin,
		Note: note, Fmt: format, Files: snaps}
	auto := rule.AutoSend()
	if auto {
		at := time.Now().Add(time.Duration(s.AutoSendDelaySec) * time.Second)
		opts.SendAt = &at
	}
	d, err := outbox.New(s.OutboxPath()).Create(opts)
	if err != nil {
		attach.Drop(s, id)
		return nil, err
	}
	fileLog := make([]*omap.Map, 0, len(snaps))
	for _, f := range snaps {
		fileLog = append(fileLog, omap.New().Set("name", f.Name).Set("size", f.Size).Set("source", f.Source))
	}
	kv := []any{"draft_id", d.ID, "chat", rule.Alias, "chars", len([]rune(text)), "files", fileLog}
	if auto {
		kv = append(kv, "auto_send_at", *d.SendAt)
	}
	_ = audit.Log(s.AuditPath(), "draft", kv...)

	filesOut := make([]*omap.Map, 0, len(d.Files))
	for _, f := range d.Files {
		filesOut = append(filesOut, omap.New().Set("name", f.Name).Set("size", f.Size))
	}
	out := omap.New().Set("draft_id", d.ID).Set("chat", rule.Alias).Set("text", d.Text).
		Set("reply_to", d.ReplyTo).Set("expires_at", d.ExpiresAt)

	if auto {
		card := s.BotReady() && Notify(ctx, s, d, rule.Title, note) != 0
		ScheduleLocal(s, d)
		when := *d.SendAt
		if at, err := time.Parse(time.RFC3339, when); err == nil {
			when = at.Local().Format("15:04:05")
		}
		return out.Set("files", filesOut).Set("send_policy", "auto_send").Set("status", "scheduled").
			Set("send_at", *d.SendAt).Set("card_sent", card).
			Set("next_step", fmt.Sprintf("В этом чате автоотправка: сообщение УЙДЁТ САМО в %s (через %d с). "+
				"Передумал — вызови tg_cancel_draft с этим draft_id до этого времени. Проверить, что ушло, — "+
				"tg_wait_approval(draft_id).", when, s.AutoSendDelaySec)), nil
	}

	switch s.SendPolicy {
	case "bot_approval":
		card := Notify(ctx, s, d, rule.Title, note)
		instruction := "Карточка с текстом ушла в бот подтверждений — человек нажмёт кнопку там. " +
			"Вызови tg_wait_approval с этим draft_id и дождись результата; " +
			"нажатие «Отправить» отправляет сообщение само."
		if card == 0 {
			instruction = "Бот подтверждений недоступен, карточку показать не удалось. " +
				"Скажи человеку и предложи подтвердить командой: " +
				fmt.Sprintf("! %s approve %s --send", config.CLIHint(), d.ID)
		}
		return out.Set("files", filesOut).Set("send_policy", s.SendPolicy).Set("card_sent", card != 0).
			Set("next_step", instruction), nil
	case "human_approval":
		return out.Set("send_policy", s.SendPolicy).Set("next_step",
			"Черновик создан, НО НЕ ОТПРАВЛЕН. Покажи пользователю текст целиком и попроси "+
				fmt.Sprintf("подтвердить командой:  ! %s approve %s\n", config.CLIHint(), d.ID)+
				"После подтверждения вызови tg_send_draft с этим draft_id."), nil
	}
	return out.Set("send_policy", s.SendPolicy).Set("next_step",
		"Черновик создан, НО НЕ ОТПРАВЛЕН. Покажи пользователю текст целиком, дождись "+
			"явного согласия в диалоге и только тогда вызови tg_send_draft "+
			"(передай в user_confirmation дословную фразу пользователя)."), nil
}

// WaitApproval — дождаться, пока человек нажмёт кнопку (или уйдёт автоотправка).
func WaitApproval(ctx context.Context, s *config.Settings, draftID string, timeoutSec int) (*omap.Map, error) {
	d, err := outbox.New(s.OutboxPath()).Get(draftID)
	if err != nil {
		return nil, err
	}
	auto := d.Status == outbox.Scheduled || d.ApprovedBy() == "auto_send"
	if s.SendPolicy != "bot_approval" && !auto {
		return nil, denied("Режим подтверждения — %s, кнопок в боте нет. "+
			"Дождись, пока человек подтвердит черновик, и вызови tg_send_draft.", s.SendPolicy)
	}
	limit := s.ApprovalTimeoutSec
	if timeoutSec > 0 && timeoutSec < limit {
		limit = timeoutSec
	}
	if auto {
		// автоотправке нужно не больше задержки и времени на саму отправку
		limit = max(limit, s.AutoSendDelaySec+60)
	}
	return WaitForDecision(ctx, s, draftID, time.Duration(limit)*time.Second)
}

// DeliverApproved — отправить черновик, который уже получил статус approved.
// Политику здесь не проверяем — она проверена там, где статус выставлялся
// (кнопка, tg approve, автоотправка). Единственная точка, где сообщение
// реально уходит в Telegram.
func DeliverApproved(ctx context.Context, s *config.Settings, draftID, by string) (*omap.Map, error) {
	box := outbox.New(s.OutboxPath())
	d, err := box.Get(draftID)
	if err != nil {
		return nil, err
	}
	rule, err := sendable(s, d.Chat)
	if err != nil {
		return nil, err
	}
	if d.Status != outbox.Approved {
		return nil, denied("Черновик %s в статусе '%s', отправить нельзя.", draftID, d.Status)
	}
	result := omap.New()
	// человек уже нажал «Отправить» — ждём сессию сколько нужно, а не падаем
	err = tgc.Run(ctx, s, tgc.Opts{Wait: 300 * time.Second, Caller: "deliver_approved"}, func(ctx context.Context, c *tgc.Conn) error {
		t, err := c.Resolve(ctx, rule)
		if err != nil {
			return err
		}
		if d.Text != "" {
			r, err := c.SendMessage(ctx, t, d.Text, d.ReplyTo, d.Fmt)
			if err != nil {
				return err
			}
			result = r
		}
		if len(d.Files) > 0 {
			paths := make([]string, len(d.Files))
			for i, f := range d.Files {
				paths[i] = f.Path
			}
			// без текста файлы отвечают на reply_to сами, с текстом — идут следом
			reply := d.ReplyTo
			if d.Text != "" {
				reply = nil
			}
			ids, err := c.SendFiles(ctx, t, paths, reply)
			if err != nil {
				return err
			}
			if _, ok := result.Get("message_id"); !ok && len(ids) > 0 {
				result.Set("message_id", ids[0])
			}
			result.Set("file_message_ids", ids)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	mid, _ := result.Get("message_id")
	msgID := toInt64(mid)
	if _, err := box.MarkSent(draftID, msgID, audit.Now()); err != nil {
		// сообщение уже ушло — ошибку записи статуса не выдаём за провал отправки
		_ = audit.Log(s.AuditPath(), "mark_sent_failed", "draft_id", draftID, "error", err.Error())
	}
	attach.Drop(s, draftID)
	names := make([]string, len(d.Files))
	for i, f := range d.Files {
		names[i] = f.Name
	}
	format, _ := result.Get("format")
	reason, _ := result.Get("fallback_reason")
	_ = audit.Log(s.AuditPath(), "send", "draft_id", draftID, "chat", rule.Alias, "message_id", msgID,
		"format", format, "fallback_reason", reason, "approved_by", by, "text", d.Text, "files", names)
	return omap.New().Set("draft_id", draftID).Set("chat", rule.Alias).Set("sent", true).Merge(result), nil
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// SendDraft — tg_send_draft / tg send.
func SendDraft(ctx context.Context, s *config.Settings, draftID, confirmation string) (*omap.Map, error) {
	box := outbox.New(s.OutboxPath())
	d, err := box.Get(draftID)
	if err != nil {
		return nil, err
	}
	if _, err := sendable(s, d.Chat); err != nil {
		return nil, err
	}
	confirmation = strings.TrimSpace(confirmation)
	if d.Status == outbox.Scheduled {
		return nil, denied("Черновик %s стоит на автоотправке и уйдёт сам — вызывать tg_send_draft не нужно. "+
			"Результат — через tg_wait_approval.", draftID)
	}
	if d.Status == outbox.Pending {
		switch s.SendPolicy {
		case "bot_approval":
			return nil, denied("Черновик %s ждёт кнопки в боте — человек ещё не нажал «Отправить». "+
				"Вызови tg_wait_approval вместо этого.", draftID)
		case "human_approval":
			return nil, denied("Черновик %s не подтверждён человеком. Нужна команда:\n  ! %s approve %s",
				draftID, config.CLIHint(), draftID)
		case "agent_confirm":
			if len([]rune(confirmation)) < 2 {
				return nil, denied("Нужно согласие пользователя: передай в user_confirmation его дословный ответ. " +
					"Выдумывать подтверждение нельзя.")
			}
			if _, err := box.Approve(draftID, "agent_confirm: "+truncate(confirmation, 200)); err != nil {
				return nil, err
			}
		}
	}
	by := truncate(confirmation, 200)
	if by == "" {
		by = "owner"
	}
	result, err := DeliverApproved(ctx, s, draftID, by)
	if err != nil {
		return nil, err
	}
	if d.BotMessageID != nil {
		CloseCard(ctx, s, draftID, result, "")
	}
	return result, nil
}

// ListDrafts — tg_list_drafts.
func ListDrafts(s *config.Settings, status string) (*omap.Map, error) {
	drafts, err := outbox.New(s.OutboxPath()).List(status)
	if err != nil {
		return nil, err
	}
	rows := make([]*omap.Map, 0, len(drafts))
	for _, d := range drafts {
		row := omap.New().Set("id", d.ID).Set("chat", d.Chat).Set("status", d.Status).Set("text", d.Text).
			Set("created_at", d.CreatedAt).Set("expires_at", d.ExpiresAt).Set("message_id", d.MessageID)
		if d.SendAt != nil {
			row.Set("send_at", *d.SendAt)
		}
		if d.SendError != nil {
			row.Set("send_error", *d.SendError)
		}
		rows = append(rows, row)
	}
	return omap.New().Set("send_policy", s.SendPolicy).Set("drafts", rows), nil
}

// CancelDraft — tg_cancel_draft / tg reject.
func CancelDraft(ctx context.Context, s *config.Settings, draftID, by string) (*omap.Map, error) {
	d, err := outbox.New(s.OutboxPath()).Cancel(draftID, by)
	if err != nil {
		return nil, err
	}
	attach.Drop(s, draftID)
	_ = audit.Log(s.AuditPath(), "cancel", "draft_id", draftID, "chat", d.Chat, "by", by)
	if d.BotMessageID != nil {
		CloseCard(ctx, s, draftID, nil, fmt.Sprintf("✋ **Отменено** (%s).", bot.MDEscape(by)))
	}
	return omap.New().Set("draft_id", draftID).Set("status", d.Status), nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
