package core

// Команды владельца в боте подтверждений. /auto — список чатов с отправкой и
// переключатели автоотправки: нажатие сразу правит config/chats.toml.
// /notes и /note — заметки к аккаунтам (core/notes.go).
//
// Меню — обычное сообщение, не rich: под rich-сообщением нажатие приходит без
// message, а id меню нужен, чтобы перерисовать кнопки.

import (
	"context"
	"fmt"
	"html"
	"strings"
	"sync"
	"time"

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
		"commands": []map[string]string{
			{"command": "auto", "description": "автоотправка по чатам"},
			{"command": "notes", "description": "заметки к аккаунтам"},
		},
		"scope": map[string]any{"type": "chat", "chat_id": s.ApprovalChatID},
	}, nil)
}

// HandleMessage — сообщение боту. Слушаем только владельца в его чате с ботом.
func HandleMessage(ctx context.Context, s *config.Settings, m *bot.Message) {
	if m.From.ID != s.ApprovalChatID || m.Chat.ID != s.ApprovalChatID {
		logEvent(s, "command_rejected", "sender", m.From.ID)
		return
	}
	text := strings.TrimSpace(m.Text)
	if !strings.HasPrefix(text, "/") {
		if acct, ok := takePendingNote(); ok && text != "" {
			addOwnerNote(ctx, s, acct, text)
		}
		return
	}
	takePendingNote() // любая команда отменяет ожидание текста заметки
	cmd, rest, _ := strings.Cut(text, " ")
	cmd, _, _ = strings.Cut(cmd, "@") // /auto@имя_бота
	switch strings.ToLower(cmd) {
	case "/auto":
		sendAutoMenu(ctx, s)
	case "/notes":
		sendNotesMenu(ctx, s)
	case "/note":
		noteCommand(ctx, s, strings.TrimSpace(rest))
	case "/start", "/help":
		say(ctx, s, "Здесь приходят карточки сообщений агентов на одобрение и запросы доступа к чатам.\n"+
			"/auto — включить или выключить автоотправку по чатам.\n"+
			"/notes — заметки к аккаунтам: агенты видят их рядом со списком чатов.")
	}
}

// say — простое сообщение владельцу (HTML).
func say(ctx context.Context, s *config.Settings, text string) {
	if err := bot.Call(ctx, s, "sendMessage", map[string]any{
		"chat_id": s.ApprovalChatID, "text": text, "parse_mode": "HTML",
	}, nil); err != nil {
		logEvent(s, "command_failed", "error", err.Error())
	}
}

// ── автоотправка ─────────────────────────────────────────────────────────

func autoMenuText(s *config.Settings) string {
	return fmt.Sprintf("🤖 <b>Автоотправка по чатам</b>\n\n"+
		"🟢 — сообщение агента уходит само через %d с, если не нажать «Отменить».\n"+
		"⚪ — только по кнопке «Отправить».\n\n"+
		"Нажми на чат, чтобы переключить. Чатов без права отправки здесь нет.", s.AutoSendDelaySec)
}

// autoKeyboard — по кнопке на чат с отправкой. В callback — alias: порядок
// чатов мог поменяться, пока меню висело. В общем боте — чаты всех
// аккаунтов, что его делят: нажатие по чату другого аккаунта основная служба
// передаст ему (метка CallbackPrefixFor), и кто бы ни перерисовал меню, оно
// выйдет тем же.
func autoKeyboard(s *config.Settings) map[string]any {
	rows := [][]map[string]string{}
	accts := menuAccounts(s)
	for _, acct := range accts {
		prefix := config.CallbackPrefixFor(acct)
		if config.IsOwnAccount(acct) && !s.SharedBot {
			prefix = "" // свой бот — свои нажатия
		}
		for _, r := range rulesOf(s, acct) {
			data := prefix + "s:" + r.Alias
			if !r.Send || len(data) > 64 {
				continue
			}
			mark := "⚪"
			if r.AutoSend() {
				mark = "🟢"
			}
			ref := r.Alias
			if len(accts) > 1 && acct != config.MainAccount {
				ref = acct + "/" + r.Alias
			}
			label := ref
			if r.Title != "" && r.Title != r.Alias {
				label = r.Title + " · " + ref
			}
			rows = append(rows, []map[string]string{{"text": mark + " " + label, "callback_data": data}})
		}
	}
	return map[string]any{"inline_keyboard": rows}
}

// menuAccounts — чьи чаты в меню бота: основной и все, кто делит его бота;
// у профиля со своим ботом — только он сам.
func menuAccounts(s *config.Settings) []string {
	own := config.AccountName()
	if own != config.MainAccount && !s.SharedBot {
		return []string{own}
	}
	out := []string{config.MainAccount}
	for _, a := range config.Accounts() {
		if a != config.MainAccount && config.SharesMainBot(a) {
			out = append(out, a)
		}
	}
	return out
}

// rulesOf — белый список аккаунта: свой — из s, чужой — из его chats.toml.
func rulesOf(s *config.Settings, acct string) []config.ChatRule {
	if config.IsOwnAccount(acct) {
		return s.Rules()
	}
	rules, order, err := config.LoadChats(config.ChatsFileOf(acct))
	if err != nil {
		return nil
	}
	out := make([]config.ChatRule, 0, len(order))
	for _, a := range order {
		out = append(out, rules[a])
	}
	return out
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

// handleAutoCallback — «s:<alias>»: переключить автоотправку своего чата
// (чужого — сюда нажатие уже передано его службе).
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
	bot.AnswerCallback(ctx, s, q.ID, "Автоотправка в "+fresh.Ref(alias)+" "+state+".")
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

// ── заметки к аккаунтам ──────────────────────────────────────────────────

// pendingNote — владелец нажал «➕»: следующее его сообщение — текст заметки.
var pendingNote struct {
	sync.Mutex
	acct  string
	until time.Time
}

func takePendingNote() (string, bool) {
	pendingNote.Lock()
	defer pendingNote.Unlock()
	acct, live := pendingNote.acct, time.Now().Before(pendingNote.until)
	pendingNote.acct = ""
	return acct, live && acct != ""
}

func addOwnerNote(ctx context.Context, s *config.Settings, acct, text string) {
	note, err := AddNote(acct, text, noteByOwner, "")
	if err != nil {
		say(ctx, s, "Не вышло: "+html.EscapeString(err.Error()))
		return
	}
	logEvent(s, "account_note_added", "account", acct, "note_id", note.ID, "by", "owner_bot",
		"chars", len([]rune(note.Text)))
	say(ctx, s, "✅ Заметка к аккаунту <b>"+html.EscapeString(acct)+"</b> добавлена — агенты увидят её в списке чатов.")
}

// noteCommand — «/note <аккаунт> <текст>» (аккаунт один — просто «/note <текст>»).
func noteCommand(ctx context.Context, s *config.Settings, rest string) {
	acct := config.MainAccount
	if config.Multi() {
		head, tail, _ := strings.Cut(rest, " ")
		a := config.FindAccount(head)
		if a == "" {
			say(ctx, s, "Напиши так: /note &lt;аккаунт&gt; &lt;текст&gt;. Аккаунты: "+
				html.EscapeString(strings.Join(config.Accounts(), ", "))+".")
			return
		}
		acct, rest = a, strings.TrimSpace(tail)
	}
	if rest == "" {
		sendNotesMenu(ctx, s)
		return
	}
	addOwnerNote(ctx, s, acct, rest)
}

const noteShown = 300 // в меню длинная заметка обрезается

func notesMenu() (string, map[string]any) {
	var b strings.Builder
	b.WriteString("📝 <b>Заметки к аккаунтам</b>\n" +
		"Агенты видят их рядом со списком чатов: чей аккаунт, от чьего имени и как там писать.\n")
	var add, del []map[string]string
	n := 0
	for _, acct := range config.Accounts() {
		b.WriteString("\n<b>" + html.EscapeString(acct) + "</b>")
		if l := AccountLabel(acct); l != "" {
			b.WriteString(" — " + html.EscapeString(l))
		}
		b.WriteString("\n")
		notes := NotesOf(acct)
		if len(notes) == 0 {
			b.WriteString("<i>заметок нет</i>\n")
		}
		for _, note := range notes {
			n++
			text := []rune(note.Text)
			if len(text) > noteShown {
				text = append(text[:noteShown], '…')
			}
			fmt.Fprintf(&b, "%d. %s <i>— %s, %s</i>\n", n, html.EscapeString(string(text)),
				html.EscapeString(note.Author()), note.At.Local().Format("02.01"))
			del = append(del, map[string]string{"text": fmt.Sprintf("🗑 %d", n), "callback_data": "n:-:" + note.ID})
		}
		add = append(add, map[string]string{"text": "➕ " + acct, "callback_data": "n:+:" + acct})
	}
	b.WriteString("\nДобавить — кнопка ➕ (потом текст сообщением) или /note")
	if config.Multi() {
		b.WriteString(" &lt;аккаунт&gt;")
	}
	b.WriteString(" &lt;текст&gt;. Убрать — 🗑 с номером.")
	rows := [][]map[string]string{}
	for len(add) > 0 {
		k := min(3, len(add))
		rows, add = append(rows, add[:k]), add[k:]
	}
	for len(del) > 0 {
		k := min(5, len(del))
		rows, del = append(rows, del[:k]), del[k:]
	}
	return b.String(), map[string]any{"inline_keyboard": rows}
}

func sendNotesMenu(ctx context.Context, s *config.Settings) {
	text, kb := notesMenu()
	if err := bot.Call(ctx, s, "sendMessage", map[string]any{
		"chat_id": s.ApprovalChatID, "text": text, "parse_mode": "HTML", "reply_markup": kb,
	}, nil); err != nil {
		logEvent(s, "command_failed", "command", "/notes", "error", err.Error())
	}
}

// handleNotesCallback — «n:+:<аккаунт>» (ждать текст заметки) и «n:-:<id>» (убрать).
func handleNotesCallback(ctx context.Context, s *config.Settings, q *bot.CallbackQuery) {
	parts := strings.SplitN(q.Data, ":", 3)
	if len(parts) < 3 {
		return
	}
	switch parts[1] {
	case "+":
		acct := config.FindAccount(parts[2])
		if acct == "" {
			bot.AnswerCallback(ctx, s, q.ID, "Аккаунта "+parts[2]+" больше нет.")
			return
		}
		pendingNote.Lock()
		pendingNote.acct, pendingNote.until = acct, time.Now().Add(10*time.Minute)
		pendingNote.Unlock()
		bot.AnswerCallback(ctx, s, q.ID, "Жду текст заметки.")
		say(ctx, s, "Пришли текст заметки к аккаунту <b>"+html.EscapeString(acct)+"</b> следующим сообщением "+
			"(в течение 10 минут). Передумал — /cancel.")
	case "-":
		acct, err := DeleteNote(parts[2], false)
		if err != nil {
			bot.AnswerCallback(ctx, s, q.ID, err.Error())
		} else {
			logEvent(s, "account_note_deleted", "account", acct, "note_id", parts[2], "by", "owner_bot")
			bot.AnswerCallback(ctx, s, q.ID, "Заметка убрана.")
		}
		if q.Message != nil {
			text, kb := notesMenu()
			_ = bot.Call(ctx, s, "editMessageText", map[string]any{
				"chat_id": s.ApprovalChatID, "message_id": q.Message.MessageID,
				"text": text, "parse_mode": "HTML", "reply_markup": kb,
			}, nil)
		}
	}
}
