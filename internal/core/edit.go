package core

// Правка отправленного агентом сообщения (tg_edit_message).
//
// Править можно только то, что ушло через tg-agent (черновик со статусом
// sent и этим message_id), и только в течение TG_EDIT_WINDOW_MIN минут
// после отправки (по умолчанию час). Правка меняет то, что уже видят
// собеседники, поэтому она — тот же черновик и тот же путь подтверждения:
// карточка «было → станет» с кнопками, в чате с автоотправкой — окно на
// отмену. Отправляет её DeliverApproved (EditOf → messages.editMessage).
// Меняется только текст, файлы не трогаются.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/tgc"
)

// editTarget — отправленный через tg-agent черновик с этим сообщением
// (последняя правка — тоже: у неё EditOf = id сообщения).
func editTarget(s *config.Settings, alias string, msgID int64) (*outbox.Draft, error) {
	drafts, err := outbox.New(s.OutboxPath()).List(outbox.Sent)
	if err != nil {
		return nil, err
	}
	var found *outbox.Draft
	for _, d := range drafts {
		if d.Chat != alias || d.EditOf != 0 || d.MessageID == nil || *d.MessageID != msgID {
			continue
		}
		found = d
	}
	if found == nil {
		return nil, denied("Сообщение %d в чате '%s' отправлено не через tg-agent — править можно только "+
			"свои сообщения (message_id из ответа tg_wait_approval / tg_send_draft).", msgID, s.Ref(alias))
	}
	if err := visibleDraft(s, found); err != nil {
		return nil, err
	}
	return found, nil
}

// EditMessage — tg_edit_message: черновик правки отправленного сообщения.
func EditMessage(ctx context.Context, s *config.Settings, chat string, messageID int64, draftID, text, note, format string) (*omap.Map, error) {
	rule, err := sendable(s, chat)
	if err != nil {
		return nil, err
	}
	if draftID != "" { // правку можно указать и черновиком, которым сообщение ушло
		d, err := outbox.New(s.OutboxPath()).Get(draftID)
		if err != nil {
			return nil, err
		}
		if d.Chat != rule.Alias || d.Status != outbox.Sent || d.MessageID == nil {
			return nil, &Bad{Msg: fmt.Sprintf("Черновик %s не отправлен в этот чат — править нечего.", draftID)}
		}
		messageID = *d.MessageID
		if d.EditOf != 0 {
			messageID = d.EditOf
		}
	}
	if messageID == 0 {
		return nil, &Bad{Msg: "Укажи message_id своего отправленного сообщения (или draft_id, которым оно ушло)."}
	}
	orig, err := editTarget(s, rule.Alias, messageID)
	if err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, &Bad{Msg: "Пустой текст: правка заменяет текст сообщения целиком — передай новый текст."}
	}
	if format == "" {
		format = "markdown"
	}
	if format != "markdown" && format != "plain" {
		return nil, &Bad{Msg: "format: только 'markdown' или 'plain'."}
	}
	if n := len([]rune(text)); n > 32768 {
		return nil, &Bad{Msg: fmt.Sprintf("Слишком длинно: %d символов, предел Telegram — 32768.", n)}
	}
	window := time.Duration(s.EditWindowMin) * time.Minute
	var old string
	err = tgc.Run(ctx, s, tgc.Opts{}, func(ctx context.Context, c *tgc.Conn) error {
		t, err := c.Resolve(ctx, rule)
		if err != nil {
			return err
		}
		m, _, err := c.Message(ctx, t, int(messageID))
		if err != nil {
			return err
		}
		msg, ok := m.(*tgc.MessageT)
		if !ok || m == nil {
			return &Bad{Msg: fmt.Sprintf("Сообщения %d больше нет (удалено) — править нечего.", messageID)}
		}
		if !msg.Out && t.Kind != "self" { // в Избранном своё приходит без out
			return denied("Сообщение %d отправлено не с этого аккаунта.", messageID)
		}
		if age := time.Since(time.Unix(int64(msg.Date), 0)); age > window {
			return denied("Сообщение %d отправлено %s назад, а править своё можно %d мин после отправки. "+
				"Нужна поправка — напиши новым сообщением.", messageID, age.Round(time.Minute), s.EditWindowMin)
		}
		if strings.TrimSpace(msg.Message) == "" && msg.Media != nil && !hasRich(msg) {
			return &Bad{Msg: "Это сообщение — файл без текста; tg_edit_message правит только текст."}
		}
		old = tgc.MessageText(msg)
		return nil
	})
	if err != nil {
		return nil, err
	}
	text, mentionNotes := fixMentions(ctx, s, rule, text, format)
	opts := outbox.CreateOpts{ID: outbox.NewID(), Chat: rule.Alias, Text: text, TTLMin: s.DraftTTLMin, Note: note,
		Fmt: format, Origin: draftOrigin(s), Topic: orig.Topic, TopicTitle: orig.TopicTitle,
		EditOf: messageID, EditOld: old}
	auto := rule.AutoSend()
	if auto {
		at := time.Now().Add(time.Duration(s.AutoSendDelaySec) * time.Second)
		opts.SendAt = &at
	}
	d, err := outbox.New(s.OutboxPath()).Create(opts)
	if err != nil {
		return nil, err
	}
	logEvent(s, "edit_draft", "draft_id", d.ID, "chat", rule.Alias, "message_id", messageID,
		"of_draft", orig.ID, "chars", len([]rune(text)))
	out := omap.New().Set("draft_id", d.ID).Set("chat", s.Ref(rule.Alias)).Set("edit_of", messageID).
		Set("old_text", old).Set("text", d.Text).Set("expires_at", d.ExpiresAt)
	if mentionNotes != nil {
		out.Set("mentions", mentionNotes)
	}
	return finishDraft(ctx, s, rule, d, out, note, auto)
}

func hasRich(m *tgc.MessageT) bool {
	_, ok := m.GetRichMessage()
	return ok
}
