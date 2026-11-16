package core

import (
	"context"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/tgc"
)

// savedFileDraft validates the owner's narrow CLI cleanup operation. It is
// not a general deletion tool or permission to remove other chats' messages.
func savedFileDraft(s *config.Settings, id string) (*outbox.Draft, config.ChatRule, error) {
	if s.Agent != nil || !s.IsTelegram() {
		return nil, config.ChatRule{}, denied("Очистка доступна только локально для Telegram-Избранного.")
	}
	rule, err := sendable(s, "saved")
	if err != nil {
		return nil, rule, err
	}
	d, err := outbox.New(s.DataDir()).Get(id)
	if err != nil {
		return nil, rule, err
	}
	if d.Chat != rule.Alias || d.Status != outbox.Sent || d.MessageID == nil || *d.MessageID <= 0 ||
		len(d.Files) != 1 || d.Text != "" || d.Poll != nil || d.Vote != nil || d.Bot != nil || d.EditOf != 0 || d.SentAt == nil {
		return nil, rule, denied("Нужен отправленный этим шлюзом черновик одного файла без отдельного текста в saved.")
	}
	at, err := time.Parse(time.RFC3339, *d.SentAt)
	if err != nil || time.Since(at) > time.Hour || at.After(time.Now().Add(time.Minute)) {
		return nil, rule, denied("Очищать так можно только файл, отправленный не больше часа назад.")
	}
	return d, rule, nil
}

// CleanupSavedFile is an owner CLI command, used only on explicit instruction.
// No MCP tool is added; a draft ID is checked before any Telegram operation.
func CleanupSavedFile(ctx context.Context, s *config.Settings, id string) (*omap.Map, error) {
	d, rule, err := savedFileDraft(s, id)
	if err != nil {
		return nil, err
	}
	err = tgc.Run(ctx, s, tgc.Opts{Caller: "cleanup_saved_file"}, func(ctx context.Context, c *tgc.Conn) error {
		t, err := c.Resolve(ctx, rule)
		if err != nil {
			return err
		}
		return cleanupSavedMessage(ctx, c, t, int(*d.MessageID))
	})
	if err != nil {
		return nil, err
	}
	logEvent(s, "saved_file_deleted", "draft_id", d.ID, "chat", rule.Alias, "message_id", *d.MessageID)
	return omap.New().Set("deleted", true).Set("draft_id", d.ID).Set("message_id", *d.MessageID), nil
}

func cleanupSavedMessage(ctx context.Context, c *tgc.Conn, t tgc.Target, id int) error {
	if t.Kind != "self" {
		return denied("Alias saved должен указывать именно на Избранное этого аккаунта.")
	}
	// Private-chat message IDs are global. The alias stored in an old draft
	// may have been rebound, so the current target alone is not enough.
	m, _, err := c.Message(ctx, t, id)
	if err != nil {
		return err
	}
	if m == nil {
		return denied("Сообщение не найдено именно в Избранном — удаление не выполняется.")
	}
	return c.DeleteMessages(ctx, t, []int{id})
}
