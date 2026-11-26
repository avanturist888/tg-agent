package core

// Удаление отправленного агентом сообщения (tg_delete_message).
//
// Удалять можно только то, что ушло через tg-agent (черновик со статусом
// sent и этим message_id, не правка, не удаление, не голос и не BotFather).
// Ограничения по времени (TG_EDIT_WINDOW_MIN) нет: устаревший статус можно
// убрать и спустя часы или дни. Сообщения владельца и чужие сообщения строго
// запрещены. Удаление проходит тот же путь подтверждения, что и отправка или
// правка: карточка с цитатой удаляемого сообщения и кнопками, в чате с
// автоотправкой — окно на отмену. Выполняет его DeliverApproved (DeleteOf →
// свежая проверка c.Message по целевому peer + DeleteMessages).

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"tgagent/internal/config"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/tgc"
)

// ownSentTarget — отправленный через tg-agent черновик исходного сообщения
// (не правка, не удаление, не голос и не BotFather). Если wantPeer != 0,
// допускается только черновик с этим PeerID.
func ownSentTarget(s *config.Settings, alias string, msgID, wantPeer int64) (*outbox.Draft, error) {
	drafts, err := outbox.New(s.DataDir()).List(outbox.Sent)
	if err != nil {
		return nil, err
	}
	var found *outbox.Draft
	for _, d := range drafts {
		if d.Chat != alias || d.EditOf != 0 || d.DeleteOf != 0 || d.Vote != nil || d.Bot != nil || d.MessageID == nil || *d.MessageID != msgID {
			continue
		}
		if wantPeer != 0 && d.PeerID != wantPeer {
			continue
		}
		found = d
	}
	if found == nil {
		return nil, denied("Сообщение %d в чате '%s' отправлено не через tg-agent — удалять можно только "+
			"свои сообщения (message_id из ответа tg_wait_approval / tg_send_draft).", msgID, s.Ref(alias))
	}
	return validateDeleteOrig(s, found, msgID)
}

func validateDeleteOrig(s *config.Settings, orig *outbox.Draft, msgID int64) (*outbox.Draft, error) {
	if err := visibleDraft(s, orig); err != nil {
		return nil, err
	}
	if orig.Scheduled {
		return nil, denied("Сообщение %d в чате '%s' ещё в очереди отложенных — дождись доставки в чат, потом удаляй.", msgID, s.Ref(orig.Chat))
	}
	if orig.Repaired {
		return nil, denied("ID сообщения %d в чате '%s' восстановлен эвристически по истории — удалять можно только сообщения с подтверждённым ID из ответа Telegram.", msgID, s.Ref(orig.Chat))
	}
	if orig.PeerID == 0 {
		return nil, denied("У сообщения %d в чате '%s' не сохранён идентификатор чата (отправлено до обновления tg-agent) — удалять можно только сообщения с подтверждённой привязкой к чату.", msgID, s.Ref(orig.Chat))
	}
	return orig, nil
}

// deleteMsgReader — чтение одного сообщения в целевом peer (tgc.Conn).
type deleteMsgReader interface {
	Message(ctx context.Context, t tgc.Target, id int) (tg.MessageClass, *tgc.Batch, error)
}

// inspectDeleteTarget проверяет в Telegram, что сообщение messageID существует
// именно в целевом peer t и отправлено с этого аккаунта (в Избранном — без out),
// и возвращает снимок его содержимого для карточки подтверждения.
func inspectDeleteTarget(ctx context.Context, c deleteMsgReader, t tgc.Target, messageID int64) (string, error) {
	m, _, err := c.Message(ctx, t, int(messageID))
	if err != nil {
		return "", err
	}
	msg, ok := m.(*tgc.MessageT)
	if !ok || m == nil {
		return "", &Bad{Msg: fmt.Sprintf("Сообщения %d нет в этом чате (возможно, уже удалено) — удалять нечего.", messageID)}
	}
	if !msg.Out && t.Kind != "self" { // в Избранном своё приходит без out
		return "", denied("Сообщение %d отправлено не с этого аккаунта — удалять можно только свои сообщения.", messageID)
	}
	old := strings.TrimSpace(tgc.MessageText(msg))
	if old == "" {
		if label := tgc.MediaLabel(msg); label != "" {
			old = "[" + label + "]"
		} else {
			old = "[без текста]"
		}
	}
	return old, nil
}

// fetchDeleteOld — свежая проверка сообщения в Telegram при создании черновика
// удаления: возвращает снимок содержимого и стабильный marked peer ID (тесты подменяют).
var fetchDeleteOld = func(ctx context.Context, s *config.Settings, rule config.ChatRule, messageID, expectedPeer int64) (old string, peerID int64, err error) {
	err = tgc.Run(ctx, s, tgc.Opts{Caller: "delete_message"}, func(ctx context.Context, c *tgc.Conn) error {
		t, err := c.Resolve(ctx, rule)
		if err != nil {
			return err
		}
		peerID = t.MarkedID()
		if expectedPeer == 0 || expectedPeer != peerID {
			return denied("Alias '%s' теперь указывает на другой чат — удалять сообщение %d по прежнему alias нельзя.", s.Ref(rule.Alias), messageID)
		}
		old, err = inspectDeleteTarget(ctx, c, t, messageID)
		return err
	})
	return old, peerID, err
}

// DeleteMessage — tg_delete_message: черновик удаления своего отправленного сообщения.
func DeleteMessage(ctx context.Context, s *config.Settings, chat string, messageID int64, draftID, note string) (*omap.Map, error) {
	if !s.IsTelegram() {
		return nil, &Bad{Msg: fmt.Sprintf("Удаление сообщений пока доступно только в Telegram (%s не поддерживается).", NetName(s.Network))}
	}
	rule, err := sendable(s, chat)
	if err != nil {
		return nil, err
	}
	var orig *outbox.Draft
	if draftID != "" { // можно указать черновик, которым сообщение ушло (или черновик его правки)
		d, err := outbox.New(s.DataDir()).Get(draftID)
		if err != nil {
			return nil, err
		}
		if err := visibleDraft(s, d); err != nil {
			return nil, err
		}
		if d.Chat != rule.Alias || d.Status != outbox.Sent || d.MessageID == nil || d.DeleteOf != 0 || d.Vote != nil || d.Bot != nil {
			return nil, &Bad{Msg: fmt.Sprintf("Черновик %s не отправлен в этот чат — удалять нечего.", draftID)}
		}
		if d.Scheduled {
			return nil, denied("Черновик %s ещё в очереди отложенных — дождись доставки в чат, потом удаляй.", draftID)
		}
		if d.Repaired {
			return nil, denied("ID сообщения черновика %s восстановлен эвристически по истории — удалять можно только сообщения с подтверждённым ID из ответа Telegram.", draftID)
		}
		if d.EditOf == 0 {
			messageID = *d.MessageID
			orig, err = validateDeleteOrig(s, d, messageID)
			if err != nil {
				return nil, err
			}
		} else {
			messageID = d.EditOf
			if d.PeerID == 0 {
				return nil, denied("У правки %s не сохранён идентификатор чата (отправлено до обновления tg-agent) — удалять можно только сообщения с подтверждённой привязкой к чату.", draftID)
			}
			orig, err = ownSentTarget(s, rule.Alias, messageID, d.PeerID)
			if err != nil {
				return nil, err
			}
		}
	} else {
		if messageID <= 0 {
			return nil, &Bad{Msg: "Укажи message_id своего отправленного сообщения (или draft_id, которым оно ушло)."}
		}
		orig, err = ownSentTarget(s, rule.Alias, messageID, 0)
		if err != nil {
			return nil, err
		}
	}
	old, peerID, err := fetchDeleteOld(ctx, s, rule, messageID, orig.PeerID)
	if err != nil {
		return nil, err
	}
	if orig.PeerID == 0 || orig.PeerID != peerID {
		return nil, denied("Alias '%s' теперь указывает на другой чат — удалять сообщение %d по прежнему alias нельзя.", s.Ref(rule.Alias), messageID)
	}
	opts := outbox.CreateOpts{
		Chat: rule.Alias, TTLMin: s.DraftTTLMin, Note: note,
		Origin: draftOrigin(s), Topic: orig.Topic, TopicTitle: orig.TopicTitle,
		DeleteOf: messageID, DeleteOld: old, DeletePeer: peerID,
	}
	auto := rule.AutoSend()
	if auto {
		at := time.Now().Add(time.Duration(s.AutoSendDelaySec) * time.Second)
		opts.SendAt = &at
	}
	d, err := outbox.New(s.DataDir()).Create(opts)
	if err != nil {
		return nil, err
	}
	logEvent(s, "delete_draft", "draft_id", d.ID, "chat", rule.Alias, "message_id", messageID, "of_draft", orig.ID)
	out := omap.New().Set("draft_id", d.ID).Set("chat", s.Ref(rule.Alias)).Set("delete_of", messageID)
	if rule.Read {
		out.Set("old_text", old)
	}
	out.Set("expires_at", d.ExpiresAt)
	return finishDraft(ctx, s, rule, d, out, note, auto)
}
