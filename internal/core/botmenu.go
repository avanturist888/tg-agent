package core

// Команды владельца в боте подтверждений. /auto — список чатов с отправкой и
// переключатели автоотправки: нажатие сразу правит config/chats.toml.
// /notes и /note — заметки к аккаунтам (core/notes.go). /updates — слежение
// за обновлениями MAX и PyMax (core/upstream.go).
//
// Меню — обычное сообщение, не rich: под rich-сообщением нажатие приходит без
// message, а id меню нужен, чтобы перерисовать кнопки.

import (
	"context"
	"errors"
	"fmt"
	"html"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"tgagent/internal/audit"
	"tgagent/internal/bot"
	"tgagent/internal/chatsfile"
	"tgagent/internal/config"
)

var botCall = bot.Call // тесты подменяют

// setCommands — подсказка команд в меню бота (в чате каждого владельца на этом боте).
func setCommands(ctx context.Context, s *config.Settings) {
	if !s.BotReady() {
		return
	}
	seen := map[int64]bool{}
	var chatIDs []int64
	addChat := func(cid int64) {
		if cid != 0 && !seen[cid] {
			seen[cid] = true
			chatIDs = append(chatIDs, cid)
		}
	}
	addChat(s.ApprovalChatID)
	for _, acct := range config.Accounts() {
		if config.IsOwnAccount(acct) {
			continue
		}
		tok, cid, _ := config.BotOf(acct)
		if tok != "" && tok == s.BotToken {
			addChat(cid)
		}
	}
	for _, cid := range chatIDs {
		setCommandsForChat(ctx, s, cid)
	}
}

func setCommandsForChat(ctx context.Context, s *config.Settings, cid int64) {
	if !s.BotReady() || cid == 0 {
		return
	}
	cmds := []map[string]string{
		{"command": "auto", "description": "автоотправка по чатам"},
		{"command": "notes", "description": "заметки к аккаунтам"},
	}
	if cid == s.ApprovalChatID && config.Profile() == "" {
		cmds = append(cmds, map[string]string{"command": "updates", "description": "обновления MAX и PyMax"})
	}
	_ = botCall(ctx, s, "setMyCommands", map[string]any{
		"commands": cmds,
		"scope":    map[string]any{"type": "chat", "chat_id": cid},
	}, nil)
}

// HandleMessage — сообщение боту. Принимаем только личные сообщения владельцев
// аккаунтов, привязанных к этому боту.
func HandleMessage(ctx context.Context, s *config.Settings, m *bot.Message) {
	accts := menuAccountsForChat(s, m.Chat.ID)
	if m.From.IsBot && len(accts) > 0 {
		return // сервисное от самого бота («закрепил сообщение») — не отказ
	}
	if m.From.ID != m.Chat.ID || len(accts) == 0 {
		logEvent(s, "command_rejected", "sender", m.From.ID)
		return
	}
	chatID := m.Chat.ID
	text := strings.TrimSpace(m.Text)
	if !strings.HasPrefix(text, "/") {
		if acct, ok := takePendingNote(chatID); ok && text != "" && slices.Contains(accts, acct) {
			addOwnerNote(ctx, s, chatID, acct, text)
		}
		return
	}
	takePendingNote(chatID) // любая команда отменяет ожидание текста заметки
	cmd, rest, _ := strings.Cut(text, " ")
	cmd, _, _ = strings.Cut(cmd, "@") // /auto@имя_бота
	switch strings.ToLower(cmd) {
	case "/auto":
		v := autoView{Filter: 'a'}
		switch strings.ToLower(strings.TrimSpace(rest)) {
		case "on", "вкл", "y":
			v.Filter = 'y'
		case "off", "выкл", "n":
			v.Filter = 'n'
		}
		sendAutoMenu(ctx, s, chatID, accts, v)
	case "/notes":
		sendNotesMenu(ctx, s, chatID, accts)
	case "/note":
		noteCommand(ctx, s, chatID, accts, strings.TrimSpace(rest))
	case "/updates":
		if chatID == s.ApprovalChatID {
			upstreamCommand(ctx, s)
		}
	case "/start", "/help":
		setCommandsForChat(ctx, s, chatID)
		msg := "Здесь приходят карточки сообщений агентов на одобрение и запросы доступа к чатам.\n"
		if config.Multi() {
			msg += "Аккаунты этого чата: <b>" + html.EscapeString(strings.Join(accts, ", ")) + "</b>.\n"
		}
		msg += "/auto — включить или выключить автоотправку по чатам (/auto вкл, /auto выкл — сразу с фильтром).\n" +
			"/notes — заметки к аккаунтам: агенты видят их рядом со списком чатов."
		if chatID == s.ApprovalChatID && config.Profile() == "" {
			msg += "\n/updates — версии MAX и PyMax; изменения приходят сами, закреплёнными."
		}
		sayTo(ctx, s, chatID, msg)
	}
}

// say — простое сообщение владельцу (HTML).
func say(ctx context.Context, s *config.Settings, text string) {
	if err := sayErr(ctx, s, text); err != nil {
		logEvent(s, "command_failed", "error", err.Error())
	}
}

// sayErr — say с ошибкой: кому важно, дошло ли (тесты подменяют).
var sayErr = func(ctx context.Context, s *config.Settings, text string) error {
	return sayToErr(ctx, s, s.ApprovalChatID, text)
}

func sayTo(ctx context.Context, s *config.Settings, chatID int64, text string) {
	if err := sayToErr(ctx, s, chatID, text); err != nil {
		logEvent(s, "command_failed", "error", err.Error())
	}
}

func sayToErr(ctx context.Context, s *config.Settings, chatID int64, text string) error {
	return botCall(ctx, s, "sendMessage", map[string]any{
		"chat_id": chatID, "text": text, "parse_mode": "HTML",
	}, nil)
}

// ── автоотправка ─────────────────────────────────────────────────────────

// autoView — что показывает меню /auto: фильтр и страница. Вид едет в
// callback каждой кнопки: нажатие по чату другого аккаунта перерисовывает
// его служба, и меню должно остаться тем же.
type autoView struct {
	Filter byte // 'a' — все, 'y' — с автоотправкой, 'n' — без неё
	Page   int  // с нуля
}

const autoPageSize = 10

func (v autoView) code() string { return fmt.Sprintf("%c%d", v.Filter, v.Page) }

// parseAutoView — вид из callback; непонятное — все чаты, первая страница.
func parseAutoView(code string) autoView {
	v := autoView{Filter: 'a'}
	if code == "" {
		return v
	}
	if f := code[0]; f == 'y' || f == 'n' {
		v.Filter = f
	}
	if n, err := strconv.Atoi(code[1:]); err == nil && n > 0 {
		v.Page = n
	}
	return v
}

// autoItem — чат в меню; data — callback без вида (его добавит autoMenu).
type autoItem struct {
	label, data string
	on          bool
}

// autoItems — чаты с отправкой для меню /auto текущего чата.
func autoItems(s *config.Settings) []autoItem {
	return autoItemsFor(s, menuAccounts(s))
}

// autoItemsFor — чаты с отправкой для указанных аккаунтов. В callback — alias:
// порядок чатов мог поменяться, пока меню висело. В общем боте нажатие по
// чату другого аккаунта служба-хост передаст ему (метка CallbackPrefixFor).
func autoItemsFor(s *config.Settings, accts []string) []autoItem {
	var out []autoItem
	for _, acct := range accts {
		prefix := config.CallbackPrefixFor(acct)
		if config.IsOwnAccount(acct) && !s.SharedBot {
			prefix = "" // свой бот — свои нажатия
		}
		for _, r := range rulesOf(s, acct) {
			data := prefix + "s:" + r.Alias
			// + «~y12» — вид меню; длиннее 64 байт Telegram кнопку не примет
			if !r.Send || len(data)+5 > 64 {
				continue
			}
			ref := r.Alias
			if len(accts) > 1 && acct != config.MainAccount {
				ref = acct + "/" + r.Alias
			}
			label := ref
			if r.Title != "" && r.Title != r.Alias {
				label = r.Title + " · " + ref
			}
			out = append(out, autoItem{label: label, data: data, on: r.AutoSend()})
		}
	}
	return out
}

// autoMenu — текст и кнопки меню /auto в виде v для чата текущего профиля.
func autoMenu(s *config.Settings, v autoView) (string, map[string]any) {
	return autoMenuItems(s, autoItems(s), v)
}

func autoMenuItems(s *config.Settings, all []autoItem, v autoView) (string, map[string]any) {
	var on, shown []autoItem
	for _, it := range all {
		if it.on {
			on = append(on, it)
		}
		if v.Filter == 'a' || (v.Filter == 'y') == it.on {
			shown = append(shown, it)
		}
	}
	pages := max(1, (len(shown)+autoPageSize-1)/autoPageSize)
	v.Page = min(v.Page, pages-1) // чаты могли убыть, пока меню висело

	filter := func(f byte, text string) map[string]string {
		if f == v.Filter {
			text = "▸ " + text
		}
		return map[string]string{"text": text, "callback_data": "am:" + autoView{Filter: f}.code()}
	}
	rows := [][]map[string]string{{
		filter('a', fmt.Sprintf("Все · %d", len(all))),
		filter('y', fmt.Sprintf("🟢 · %d", len(on))),
		filter('n', fmt.Sprintf("⚪ · %d", len(all)-len(on))),
	}}
	for _, it := range shown[min(len(shown), v.Page*autoPageSize):min(len(shown), (v.Page+1)*autoPageSize)] {
		mark := "⚪"
		if it.on {
			mark = "🟢"
		}
		rows = append(rows, []map[string]string{{"text": mark + " " + it.label, "callback_data": it.data + "~" + v.code()}})
	}
	if pages > 1 {
		nav := []map[string]string{}
		if v.Page > 0 {
			nav = append(nav, map[string]string{"text": "◀️", "callback_data": "am:" + autoView{v.Filter, v.Page - 1}.code()})
		}
		nav = append(nav, map[string]string{"text": fmt.Sprintf("%d / %d", v.Page+1, pages), "callback_data": "am:" + v.code()})
		if v.Page < pages-1 {
			nav = append(nav, map[string]string{"text": "▶️", "callback_data": "am:" + autoView{v.Filter, v.Page + 1}.code()})
		}
		rows = append(rows, nav)
	}

	what := map[byte]string{'a': "все чаты", 'y': "с автоотправкой", 'n': "без автоотправки"}[v.Filter]
	text := fmt.Sprintf("🤖 <b>Автоотправка по чатам</b>\n\n"+
		"🟢 — сообщение агента уходит само через %d с, если не нажать «Отменить».\n"+
		"⚪ — только по кнопке «Отправить».\n\n"+
		"Показаны: %s", s.AutoSendDelaySec, what)
	switch {
	case len(shown) == 0:
		text += " — таких нет."
	case pages > 1:
		text += fmt.Sprintf(", стр. %d из %d.", v.Page+1, pages)
	default:
		text += "."
	}
	text += " Нажми на чат, чтобы переключить. Чатов без права отправки здесь нет."
	return text, map[string]any{"inline_keyboard": rows}
}

// menuAccountsForChat — все аккаунты на этом боте (с тем же BotToken), у
// которых разрешённый ApprovalChatID == chatID, в стабильном порядке Accounts().
func menuAccountsForChat(s *config.Settings, chatID int64) []string {
	if chatID == 0 {
		return nil
	}
	var out []string
	for _, acct := range config.Accounts() {
		if config.IsOwnAccount(acct) {
			if s.ApprovalChatID == chatID {
				out = append(out, acct)
			}
			continue
		}
		if s.BotToken == "" {
			continue
		}
		tok, cid, _ := config.BotOf(acct)
		if tok != "" && tok == s.BotToken && cid == chatID {
			out = append(out, acct)
		}
	}
	return out
}

// menuAccounts — чьи чаты в меню бота для s.ApprovalChatID.
func menuAccounts(s *config.Settings) []string {
	if s.ApprovalChatID != 0 {
		if accts := menuAccountsForChat(s, s.ApprovalChatID); len(accts) > 0 {
			return accts
		}
	}
	return []string{config.AccountName()}
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

func sendAutoMenu(ctx context.Context, s *config.Settings, chatID int64, accts []string, v autoView) {
	items := autoItemsFor(s, accts)
	text, kb := autoMenuItems(s, items, v)
	if len(items) == 0 {
		text = "Ни в один чат агентам не разрешено писать — автоотправку включать не для чего.\n" +
			"Отправку открывает " + html.EscapeString("tg allow <alias> --send") + "."
	}
	if err := botCall(ctx, s, "sendMessage", map[string]any{
		"chat_id": chatID, "text": text, "parse_mode": "HTML", "reply_markup": kb,
	}, nil); err != nil {
		logEvent(s, "command_failed", "command", "/auto", "error", err.Error())
	}
}

// handleAutoMenuCallback — «am:<вид>»: фильтр или другая страница меню /auto.
func handleAutoMenuCallback(ctx context.Context, s *config.Settings, q *bot.CallbackQuery) {
	answerCallback(ctx, s, q.ID, "")
	if q.Message == nil {
		return
	}
	fresh, err := config.Load()
	if err != nil {
		return
	}
	accts := menuAccountsForChat(fresh, q.From.ID)
	if len(accts) == 0 {
		return
	}
	editAutoMenu(ctx, fresh, q.From.ID, accts, q.Message.MessageID, parseAutoView(strings.TrimPrefix(q.Data, "am:")))
}

func editAutoMenu(ctx context.Context, s *config.Settings, chatID int64, accts []string, messageID int64, v autoView) {
	text, kb := autoMenuItems(s, autoItemsFor(s, accts), v)
	_ = botCall(ctx, s, "editMessageText", map[string]any{
		"chat_id": chatID, "message_id": messageID, "text": text, "parse_mode": "HTML", "reply_markup": kb,
	}, nil)
}

// handleAutoCallback — «s:<alias>~<вид>»: переключить автоотправку своего
// чата (чужого — сюда нажатие уже передано его службе) и перерисовать меню в
// том же виде. Кнопки старых меню — без вида.
func handleAutoCallback(ctx context.Context, s *config.Settings, q *bot.CallbackQuery) {
	alias, code, _ := strings.Cut(strings.TrimPrefix(q.Data, "s:"), "~")
	fresh, err := config.Load() // права могли поменять, пока меню висело
	if err != nil {
		answerCallback(ctx, s, q.ID, "Не вышло: "+err.Error())
		return
	}
	on, err := toggleAuto(fresh, alias)
	if err != nil {
		answerCallback(ctx, s, q.ID, err.Error())
		logEvent(s, "auto_send_change_failed", "chat", alias, "error", err.Error())
		return
	}
	logEvent(s, "auto_send_changed", "chat", alias, "auto_send", on, "by", "telegram_button")
	state := "выключена — только по кнопке"
	if on {
		state = "включена"
	}
	answerCallback(ctx, s, q.ID, "Автоотправка в "+fresh.Ref(alias)+" "+state+".")
	if q.Message == nil {
		return
	}
	if fresh, err = config.Load(); err != nil {
		return
	}
	editAutoMenu(ctx, fresh, fresh.ApprovalChatID, menuAccountsForChat(fresh, fresh.ApprovalChatID), q.Message.MessageID, parseAutoView(code))
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

type pendingNoteEntry struct {
	acct  string
	until time.Time
}

// pendingNote — владелец нажал «➕»: следующее его сообщение — текст заметки.
// Хранится отдельно на каждый chat_id, чтобы разные чаты общего бота не
// перебивали друг друга.
var pendingNote struct {
	sync.Mutex
	byChat map[int64]pendingNoteEntry
}

func setPendingNote(chatID int64, acct string, until time.Time) {
	pendingNote.Lock()
	defer pendingNote.Unlock()
	if pendingNote.byChat == nil {
		pendingNote.byChat = map[int64]pendingNoteEntry{}
	}
	pendingNote.byChat[chatID] = pendingNoteEntry{acct: acct, until: until}
}

func takePendingNote(chatID int64) (string, bool) {
	pendingNote.Lock()
	defer pendingNote.Unlock()
	entry, ok := pendingNote.byChat[chatID]
	delete(pendingNote.byChat, chatID)
	if !ok {
		return "", false
	}
	return entry.acct, time.Now().Before(entry.until) && entry.acct != ""
}

func addOwnerNote(ctx context.Context, s *config.Settings, chatID int64, acct, text string) {
	note, err := AddNote(acct, text, noteByOwner, "")
	if err != nil {
		sayTo(ctx, s, chatID, "Не вышло: "+html.EscapeString(err.Error()))
		return
	}
	logEvent(s, "account_note_added", "account", acct, "note_id", note.ID, "by", "owner_bot",
		"chars", len([]rune(note.Text)))
	sayTo(ctx, s, chatID, "✅ Заметка к аккаунту <b>"+html.EscapeString(acct)+"</b> добавлена — агенты увидят её в списке чатов.")
}

func findInAccounts(accts []string, name string) string {
	name = strings.TrimSpace(name)
	for _, a := range accts {
		if strings.EqualFold(a, name) {
			return a
		}
	}
	return ""
}

// noteCommand — «/note <аккаунт> <текст>» (аккаунт в чате один — просто «/note <текст>»).
func noteCommand(ctx context.Context, s *config.Settings, chatID int64, accts []string, rest string) {
	var acct string
	if len(accts) == 1 {
		acct = accts[0]
	} else {
		head, tail, _ := strings.Cut(rest, " ")
		a := findInAccounts(accts, head)
		if a == "" {
			sayTo(ctx, s, chatID, "Напиши так: /note &lt;аккаунт&gt; &lt;текст&gt;. Аккаунты: "+
				html.EscapeString(strings.Join(accts, ", "))+".")
			return
		}
		acct, rest = a, strings.TrimSpace(tail)
	}
	if rest == "" {
		sendNotesMenu(ctx, s, chatID, accts)
		return
	}
	addOwnerNote(ctx, s, chatID, acct, rest)
}

const noteShown = 300 // в меню длинная заметка обрезается

func notesMenu() (string, map[string]any) {
	return notesMenuFor(config.Accounts())
}

func notesMenuFor(accts []string) (string, map[string]any) {
	var b strings.Builder
	b.WriteString("📝 <b>Заметки к аккаунтам</b>\n" +
		"Агенты видят их рядом со списком чатов: чей аккаунт, от чьего имени и как там писать.\n")
	var add, del []map[string]string
	n := 0
	for _, acct := range accts {
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
	if len(accts) > 1 {
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

func sendNotesMenu(ctx context.Context, s *config.Settings, chatID int64, accts []string) {
	text, kb := notesMenuFor(accts)
	if err := botCall(ctx, s, "sendMessage", map[string]any{
		"chat_id": chatID, "text": text, "parse_mode": "HTML", "reply_markup": kb,
	}, nil); err != nil {
		logEvent(s, "command_failed", "command", "/notes", "error", err.Error())
	}
}

// handleNotesCallback — «n:+:<аккаунт>» (ждать текст заметки) и «n:-:<id>» (убрать).
func handleNotesCallback(ctx context.Context, s *config.Settings, q *bot.CallbackQuery, accts []string) {
	parts := strings.SplitN(q.Data, ":", 3)
	if len(parts) < 3 {
		return
	}
	switch parts[1] {
	case "+":
		acct := config.FindAccount(parts[2])
		if acct == "" {
			answerCallback(ctx, s, q.ID, "Аккаунта "+parts[2]+" больше нет.")
			return
		}
		if !slices.Contains(accts, acct) {
			answerCallback(ctx, s, q.ID, "Эта кнопка не для вас.")
			_ = audit.Log(s.AuditPath(), "callback_rejected", "sender", q.From.ID, "account", acct, "data", q.Data)
			return
		}
		setPendingNote(q.From.ID, acct, time.Now().Add(10*time.Minute))
		answerCallback(ctx, s, q.ID, "Жду текст заметки.")
		sayTo(ctx, s, q.From.ID, "Пришли текст заметки к аккаунту <b>"+html.EscapeString(acct)+"</b> следующим сообщением "+
			"(в течение 10 минут). Передумал — /cancel.")
	case "-":
		acct, err := deleteNoteFor(parts[2], accts)
		if err != nil {
			var d *Denied
			if errors.As(err, &d) {
				answerCallback(ctx, s, q.ID, "Эта кнопка не для вас.")
				_ = audit.Log(s.AuditPath(), "callback_rejected", "sender", q.From.ID, "data", q.Data)
				return
			}
			answerCallback(ctx, s, q.ID, err.Error())
		} else {
			logEvent(s, "account_note_deleted", "account", acct, "note_id", parts[2], "by", "owner_bot")
			answerCallback(ctx, s, q.ID, "Заметка убрана.")
		}
		if q.Message != nil {
			text, kb := notesMenuFor(accts)
			_ = botCall(ctx, s, "editMessageText", map[string]any{
				"chat_id": q.From.ID, "message_id": q.Message.MessageID,
				"text": text, "parse_mode": "HTML", "reply_markup": kb,
			}, nil)
		}
	}
}
