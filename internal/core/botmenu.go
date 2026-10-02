package core

// Команды владельца в боте подтверждений. /auto — список чатов с отправкой и
// переключатели автоотправки: нажатие сразу правит config/chats.toml.
//
// Меню — обычное сообщение, не rich: под rich-сообщением нажатие приходит без
// message, а id меню нужен, чтобы перерисовать кнопки.

import (
	"context"
	"fmt"
	"html"
	"strings"

	"tgagent/internal/bot"
	"tgagent/internal/chatsfile"
	"tgagent/internal/config"
)

// setCommands — подсказка команд в меню бота (только в чате владельца).
func setCommands(ctx context.Context, s *config.Settings) {
	if !s.BotReady() {
		return
	}
	_ = bot.Call(ctx, s, "setMyCommands", map[string]any{
		"commands": []map[string]string{{"command": "auto", "description": "автоотправка по чатам"}},
		"scope":    map[string]any{"type": "chat", "chat_id": s.ApprovalChatID},
	}, nil)
}

// HandleMessage — сообщение боту. Слушаем только владельца в его чате с ботом.
func HandleMessage(ctx context.Context, s *config.Settings, m *bot.Message) {
	if m.From.ID != s.ApprovalChatID || m.Chat.ID != s.ApprovalChatID {
		logEvent(s, "command_rejected", "sender", m.From.ID)
		return
	}
	cmd, _, _ := strings.Cut(strings.TrimSpace(m.Text), " ")
	cmd, _, _ = strings.Cut(cmd, "@") // /auto@имя_бота
	switch strings.ToLower(cmd) {
	case "/auto":
		sendAutoMenu(ctx, s)
	case "/start", "/help":
		_ = bot.Call(ctx, s, "sendMessage", map[string]any{
			"chat_id": s.ApprovalChatID,
			"text": "Здесь приходят карточки сообщений агентов на одобрение и запросы доступа к чатам.\n" +
				"/auto — включить или выключить автоотправку по чатам.",
		}, nil)
	}
}

func autoMenuText(s *config.Settings) string {
	return fmt.Sprintf("🤖 <b>Автоотправка по чатам</b>\n\n"+
		"🟢 — сообщение агента уходит само через %d с, если не нажать «Отменить».\n"+
		"⚪ — только по кнопке «Отправить».\n\n"+
		"Нажми на чат, чтобы переключить. Чатов без права отправки здесь нет.", s.AutoSendDelaySec)
}

// autoKeyboard — по кнопке на чат с отправкой. В callback — alias: порядок
// чатов мог поменяться, пока меню висело.
func autoKeyboard(s *config.Settings) map[string]any {
	rows := [][]map[string]string{}
	for _, r := range s.Rules() {
		data := "s:" + r.Alias
		if !r.Send || len(data) > 64 {
			continue
		}
		mark := "⚪"
		if r.AutoSend() {
			mark = "🟢"
		}
		label := r.Alias
		if r.Title != "" && r.Title != r.Alias {
			label = r.Title + " · " + r.Alias
		}
		rows = append(rows, []map[string]string{{"text": mark + " " + label, "callback_data": data}})
	}
	return map[string]any{"inline_keyboard": rows}
}

func sendAutoMenu(ctx context.Context, s *config.Settings) {
	text := autoMenuText(s)
	kb := autoKeyboard(s)
	if len(kb["inline_keyboard"].([][]map[string]string)) == 0 {
		text = "Ни в один чат агентам не разрешено писать — автоотправку включать не для чего.\n" +
			"Отправку открывает " + html.EscapeString("tg allow <alias> --send") + "."
	}
	if err := bot.Call(ctx, s, "sendMessage", map[string]any{
		"chat_id": s.ApprovalChatID, "text": text, "parse_mode": "HTML", "reply_markup": kb,
	}, nil); err != nil {
		logEvent(s, "command_failed", "command", "/auto", "error", err.Error())
	}
}

// handleAutoCallback — «s:<alias>»: переключить автоотправку чата.
func handleAutoCallback(ctx context.Context, s *config.Settings, q *bot.CallbackQuery) {
	alias := strings.TrimPrefix(q.Data, "s:")
	fresh, err := config.Load() // права могли поменять, пока меню висело
	if err != nil {
		bot.AnswerCallback(ctx, s, q.ID, "Не вышло: "+err.Error())
		return
	}
	on, err := toggleAuto(fresh, alias)
	if err != nil {
		bot.AnswerCallback(ctx, s, q.ID, err.Error())
		logEvent(s, "auto_send_change_failed", "chat", alias, "error", err.Error())
		return
	}
	logEvent(s, "auto_send_changed", "chat", alias, "auto_send", on, "by", "telegram_button")
	state := "выключена — только по кнопке"
	if on {
		state = "включена"
	}
	bot.AnswerCallback(ctx, s, q.ID, "Автоотправка в "+alias+" "+state+".")
	if q.Message == nil {
		return
	}
	if fresh, err = config.Load(); err != nil {
		return
	}
	_ = bot.Call(ctx, s, "editMessageReplyMarkup", map[string]any{
		"chat_id": s.ApprovalChatID, "message_id": q.Message.MessageID, "reply_markup": autoKeyboard(fresh),
	}, nil)
}

// toggleAuto — переключить автоотправку чата в chats.toml; вернёт новое состояние.
func toggleAuto(s *config.Settings, alias string) (bool, error) {
	rule, ok := s.Chats[alias]
	if !ok {
		return false, fmt.Errorf("Чата %s уже нет в белом списке.", alias)
	}
	if !rule.Send {
		return false, fmt.Errorf("В %s агентам писать нельзя — сначала tg allow %s --send.", alias, alias)
	}
	on := !rule.AutoSend()
	return on, chatsfile.Update(alias, rule.Read, rule.Send, on, nil, nil)
}
