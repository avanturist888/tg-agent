package core

// Папки Telegram и запросы доступа к чатам.
//
// Агент видит папки владельца и чаты в них — только название, тип и
// счётчик непрочитанных, без сообщений — и может попросить доступ к чату.
// Решает владелец кнопкой в боте; согласие дописывает чат в белый список
// (config/chats.toml), отказ ничего не меняет. Агентам из контейнеров это
// недоступно: их белый список урезан, а папки раскрыли бы все чаты.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"tgagent/internal/bot"
	"tgagent/internal/chatsfile"
	"tgagent/internal/config"
	"tgagent/internal/inbox"
	"tgagent/internal/lock"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/tgc"
)

const remoteNoFolders = "Папки и запросы доступа недоступны агенту из контейнера: его чаты задаёт " +
	"владелец в config/agents.toml. Нужен ещё чат — попроси владельца."

const (
	accessPending = "pending"
	accessGranted = "granted"
	accessDenied  = "denied"
)

func onlyLocal(s *config.Settings) error {
	if s.Agent != nil {
		return denied("%s", remoteNoFolders)
	}
	return nil
}

// ── папки ────────────────────────────────────────────────────────────────

// ListFolders — tg_list_folders.
func ListFolders(ctx context.Context, s *config.Settings) (*omap.Map, error) {
	if err := onlyLocal(s); err != nil {
		return nil, err
	}
	var folders []tgc.Folder
	err := tgc.Run(ctx, s, tgc.Opts{Caller: "folders"}, func(ctx context.Context, c *tgc.Conn) error {
		var err error
		folders, err = c.Folders(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	list := make([]*omap.Map, 0, len(folders))
	for _, f := range folders {
		item := omap.New().Set("id", f.ID).Set("title", f.Title)
		if f.Emoticon != "" {
			item.Set("emoji", f.Emoticon)
		}
		if f.Shared {
			item.Set("shared", true)
		}
		item.Set("pinned_and_included", len(f.Pinned)+len(f.Include))
		if rules := f.Rules(); len(rules) > 0 {
			item.Set("rules", rules)
		}
		list = append(list, item)
	}
	logEvent(s, "folders_listed", "count", len(folders))
	return omap.New().Set("folders", list).Set("hint",
		"Чаты папки — tg_folder_chats(folder=<id или название>)."), nil
}

// FolderChats — tg_folder_chats.
func FolderChats(ctx context.Context, s *config.Settings, folder string) (*omap.Map, error) {
	if err := onlyLocal(s); err != nil {
		return nil, err
	}
	folder = strings.TrimSpace(folder)
	if folder == "" {
		return nil, &Bad{Msg: "Укажи папку: id или название из tg_list_folders."}
	}
	var (
		found tgc.Folder
		rows  []tgc.DialogRow
		known = map[int64]config.ChatRule{} // id → правило белого списка
	)
	err := tgc.Run(ctx, s, tgc.Opts{Wait: 30 * time.Second, Caller: "folder_chats"}, func(ctx context.Context, c *tgc.Conn) error {
		folders, err := c.Folders(ctx)
		if err != nil {
			return err
		}
		var titles []string
		ok := false
		for _, f := range folders {
			titles = append(titles, fmt.Sprintf("%d «%s»", f.ID, f.Title))
			if strconv.Itoa(f.ID) == folder || strings.EqualFold(f.Title, folder) {
				found, ok = f, true
			}
		}
		if !ok {
			return &Bad{Msg: fmt.Sprintf("Папки «%s» нет. Есть: %s.", folder, strings.Join(titles, ", "))}
		}
		if rows, err = c.FolderChats(ctx, found, 0); err != nil {
			return err
		}
		for _, r := range s.Rules() {
			if id := ruleMarkedID(r, c); id != 0 {
				known[id] = r
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	pending := map[int64]string{}
	for _, r := range loadAccess(accessPath(s)) {
		if r.Status == accessPending {
			pending[r.Peer] = r.ID
		}
	}
	chats := make([]*omap.Map, 0, len(rows))
	for _, r := range rows {
		item := omap.New().Set("id", r.ID).Set("title", r.Title).Set("kind", r.Kind)
		if r.Username != "" {
			item.Set("username", "@"+r.Username)
		}
		item.Set("unread", r.Unread)
		if rule, ok := known[r.ID]; ok {
			item.Set("alias", s.Ref(rule.Alias)).Set("can_read", rule.Read).Set("can_send", rule.Send)
		} else {
			item.Set("alias", nil)
		}
		if id, ok := pending[r.ID]; ok {
			item.Set("access_request", id)
		}
		chats = append(chats, item)
	}
	logEvent(s, "folder_chats_listed", "folder", found.Title, "count", len(rows))
	return omap.New().Set("folder", omap.New().Set("id", found.ID).Set("title", found.Title)).
		Set("count", len(chats)).Set("chats", chats).Set("hint",
		"alias = null — чата нет в белом списке. Нужен доступ — tg_request_access(chat_id=id, reason=…)."), nil
}

// ── запросы доступа ──────────────────────────────────────────────────────

type accessRequest struct {
	ID       string      `json:"id"`
	Peer     int64       `json:"peer"`
	Title    string      `json:"title"`
	Kind     string      `json:"kind"`
	Username string      `json:"username,omitempty"`
	Read     bool        `json:"read"`
	Send     bool        `json:"send"`
	Reason   string      `json:"reason"`
	Status   string      `json:"status"`
	Alias    string      `json:"alias,omitempty"` // какой alias у чата сейчас / стал после согласия
	CanRead  bool        `json:"can_read,omitempty"`
	CanSend  bool        `json:"can_send,omitempty"`
	Agent    *inbox.Addr `json:"agent,omitempty"` // куда сообщить о решении
	CardID   int64       `json:"card_id,omitempty"`
	Created  time.Time   `json:"created"`
	Decided  time.Time   `json:"decided,omitzero"`
}

var accessMu sync.Mutex

func accessPath(s *config.Settings) string { return filepath.Join(s.DataDir(), "access_requests.json") }

func loadAccess(path string) map[string]*accessRequest {
	out := map[string]*accessRequest{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// editAccess — прочитать, поменять и записать запросы под замком.
func editAccess(s *config.Settings, fn func(m map[string]*accessRequest) bool) error {
	accessMu.Lock()
	defer accessMu.Unlock()
	path := accessPath(s)
	m := loadAccess(path)
	if !fn(m) {
		return nil
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func rights(read, send bool) string {
	switch {
	case read && send:
		return "чтение и отправка"
	case send:
		return "только отправка"
	default:
		return "только чтение"
	}
}

// RequestAccess — tg_request_access: карточка владельцу в бот.
func RequestAccess(ctx context.Context, s *config.Settings, chatID int64, read, send bool, reason string) (*omap.Map, error) {
	if err := onlyLocal(s); err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, &Bad{Msg: "reason обязателен: по нему владелец решает, давать ли доступ. Напиши, зачем чат и для какого проекта."}
	}
	if chatID == 0 {
		return nil, &Bad{Msg: "chat_id — id чата из tg_folder_chats."}
	}
	if !read && !send {
		read = true
	}
	if !s.BotReady() {
		return nil, &Bad{Msg: "Бот подтверждений не настроен — запросить доступ кнопкой нельзя. " +
			"Попроси владельца открыть чат самому: tg allow <alias> --id " + strconv.FormatInt(chatID, 10)}
	}
	var (
		info     tgc.DialogRow
		existing *config.ChatRule
	)
	err := tgc.Run(ctx, s, tgc.Opts{Wait: 30 * time.Second, Caller: "request_access"}, func(ctx context.Context, c *tgc.Conn) error {
		var err error
		if info, err = c.ChatInfo(ctx, chatID); err != nil {
			return &Bad{Msg: fmt.Sprintf("Чат %d не найден среди чатов аккаунта: %v", chatID, err)}
		}
		for _, r := range s.Rules() {
			if ruleMarkedID(r, c) == chatID {
				existing = &r
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := omap.New().Set("chat_id", chatID).Set("title", info.Title)
	if existing != nil && (existing.Read || !read) && (existing.Send || !send) {
		return out.Set("status", "already_allowed").Set("alias", s.Ref(existing.Alias)).
			Set("can_read", existing.Read).Set("can_send", existing.Send), nil
	}
	// такой же запрос ещё ждёт кнопки — вторую карточку не шлём
	for _, r := range loadAccess(accessPath(s)) {
		if r.Status == accessPending && r.Peer == chatID && (r.Read || !read) && (r.Send || !send) {
			return out.Set("request_id", r.ID).Set("status", accessPending).Set("next_step",
				"Такой запрос уже ждёт владельца. Дождись: tg_wait_access(request_id)."), nil
		}
	}
	req := &accessRequest{ID: outbox.NewID(), Peer: chatID, Title: info.Title, Kind: info.Kind, Username: info.Username,
		Read: read, Send: send, Reason: reason, Status: accessPending, Agent: agentFrom(ctx), Created: time.Now()}
	if existing != nil {
		req.Alias = existing.Alias
	}
	card, err := sendAccessCard(ctx, s, req, existing)
	if err != nil {
		return nil, fmt.Errorf("карточка в бот не ушла: %w", err)
	}
	req.CardID = card
	if err := editAccess(s, func(m map[string]*accessRequest) bool { m[req.ID] = req; return true }); err != nil {
		return nil, err
	}
	logEvent(s, "access_requested", "request_id", req.ID, "chat_id", chatID, "title", info.Title,
		"read", read, "send", send, "reason", reason)
	return out.Set("request_id", req.ID).Set("status", accessPending).Set("asked", rights(read, send)).
		Set("next_step", "Карточка ушла владельцу в бот. Когда он нажмёт кнопку, сюда придёт уведомление "+
			"(если сессия жива); дождаться явно — tg_wait_access(request_id). Не проси повторно."), nil
}

func kindRu(kind string) string {
	switch kind {
	case "user":
		return "личка"
	case "bot":
		return "бот"
	case "group":
		return "группа"
	case "channel":
		return "канал"
	}
	return kind
}

func accessCardMarkdown(r *accessRequest, existing *config.ChatRule, status string) string {
	var b strings.Builder
	b.WriteString("🔑 **Агент просит доступ к чату**\n\n")
	chat := "«" + bot.MDEscape(r.Title) + "» — " + kindRu(r.Kind)
	if r.Username != "" {
		chat += ", @" + bot.MDEscape(r.Username)
	}
	fmt.Fprintf(&b, "**Чат:** %s\n", chat)
	fmt.Fprintf(&b, "**Просит:** %s", rights(r.Read, r.Send))
	if r.Send {
		b.WriteString(" (каждое сообщение — всё равно через карточку)")
	}
	b.WriteString("\n")
	if existing != nil {
		fmt.Fprintf(&b, "**Сейчас:** %s (`%s`)\n", rights(existing.Read, existing.Send), existing.Alias)
	}
	fmt.Fprintf(&b, "**Зачем:** %s\n", bot.MDEscape(r.Reason))
	if status != "" {
		b.WriteString("\n" + status)
	}
	return b.String()
}

func accessKeyboard(s *config.Settings, r *accessRequest) map[string]any {
	grant := "r"
	if r.Send {
		grant = "rw"
	}
	p := s.CallbackPrefix() + "a:" + r.ID + ":"
	rows := [][]map[string]string{{{"text": "✅ Разрешить", "callback_data": p + grant}}}
	if r.Send && r.Read {
		rows = append(rows, []map[string]string{{"text": "👁 Только чтение", "callback_data": p + "r"}})
	}
	rows = append(rows, []map[string]string{{"text": "✋ Отказать", "callback_data": p + "no"}})
	return map[string]any{"inline_keyboard": rows}
}

func sendAccessCard(ctx context.Context, s *config.Settings, r *accessRequest, existing *config.ChatRule) (int64, error) {
	var msg struct {
		MessageID int64 `json:"message_id"`
	}
	err := bot.Call(ctx, s, "sendRichMessage", map[string]any{
		"chat_id":      s.ApprovalChatID,
		"reply_markup": accessKeyboard(s, r),
		"rich_message": map[string]string{"markdown": accessCardMarkdown(r, existing, "")},
	}, &msg)
	return msg.MessageID, err
}

// finishAccessCard — итог в карточке, кнопки снимаются.
func finishAccessCard(ctx context.Context, s *config.Settings, r *accessRequest, status string) {
	if r.CardID == 0 {
		return
	}
	err := bot.Call(ctx, s, "editMessageText", map[string]any{
		"chat_id": s.ApprovalChatID, "message_id": r.CardID,
		"rich_message": map[string]string{"markdown": accessCardMarkdown(r, nil, status)},
	}, nil)
	if err != nil {
		logEvent(s, "card_edit_failed", "request_id", r.ID, "error", err.Error())
		_ = bot.Call(ctx, s, "editMessageReplyMarkup", map[string]any{"chat_id": s.ApprovalChatID, "message_id": r.CardID}, nil)
	}
}

// handleAccessCallback — нажатие под карточкой запроса доступа («a:<id>:<rw|r|no>»).
func handleAccessCallback(ctx context.Context, s *config.Settings, q *bot.CallbackQuery) {
	parts := strings.SplitN(q.Data, ":", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	id, verdict := parts[1], parts[2]
	r := loadAccess(accessPath(s))[id]
	if r == nil {
		bot.AnswerCallback(ctx, s, q.ID, "Запрос не найден.")
		return
	}
	if r.Status != accessPending {
		bot.AnswerCallback(ctx, s, q.ID, "Уже решено.")
		return
	}
	var status string
	switch verdict {
	case "rw", "r":
		read, send := true, verdict == "rw"
		if !r.Read && verdict == "rw" {
			read = false // просили только отправку
		}
		fresh, err := config.Load() // белый список свежий: его могли поправить, пока карточка висела
		if err != nil {
			bot.AnswerCallback(ctx, s, q.ID, "Не вышло: "+err.Error())
			return
		}
		alias, err := grantAccess(fresh, r, read, send)
		if err != nil {
			bot.AnswerCallback(ctx, s, q.ID, "Не вышло: "+err.Error())
			logEvent(s, "access_grant_failed", "request_id", id, "error", err.Error())
			return
		}
		r.Status, r.Alias, r.CanRead, r.CanSend = accessGranted, alias, read, send
		status = fmt.Sprintf("✅ **Разрешено:** %s, alias `%s`", rights(read, send), alias)
		bot.AnswerCallback(ctx, s, q.ID, "Чат открыт агентам.")
	default:
		r.Status = accessDenied
		status = "✋ **Отказано** — белый список не менялся."
		bot.AnswerCallback(ctx, s, q.ID, "Отказано.")
	}
	r.Decided = time.Now()
	_ = editAccess(s, func(m map[string]*accessRequest) bool {
		if cur := m[id]; cur != nil && cur.Status == accessPending {
			m[id] = r
			return true
		}
		return false
	})
	logEvent(s, "access_"+r.Status, "request_id", id, "chat_id", r.Peer, "title", r.Title, "alias", r.Alias,
		"read", r.CanRead, "send", r.CanSend, "by", "telegram_button")
	finishAccessCard(ctx, s, r, status)
	go afterAccess(context.WithoutCancel(ctx), r)
}

// grantAccess — дописать чат в белый список (или расширить права). Возвращает alias.
func grantAccess(s *config.Settings, r *accessRequest, read, send bool) (string, error) {
	for _, rule := range s.Rules() {
		if rulePeerID(rule) == r.Peer {
			return rule.Alias, chatsfile.Update(rule.Alias, rule.Read || read, rule.Send || send, rule.AutoSend(), nil, nil)
		}
	}
	alias := uniqueAlias(s, slug(r.Title, r.Kind))
	note := fmt.Sprintf("Открыт по запросу агента %s: %s", time.Now().Format("02.01.2006"), r.Reason)
	return alias, chatsfile.Add(chatsfile.Chat{Alias: alias, Peer: r.Peer, Title: r.Title, Read: read, Send: send, Note: note})
}

// rulePeerID — id чата из правила без обращения к Telegram (0 — задан не числом).
func rulePeerID(rule config.ChatRule) int64 {
	switch p := rule.Peer.(type) {
	case int64:
		return p
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		return n
	}
	return 0
}

// afterAccess — сообщить агенту о решении и сразу завести ленту нового чата.
func afterAccess(ctx context.Context, r *accessRequest) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	s, err := config.Load()
	if err != nil {
		return
	}
	if r.Status == accessGranted && r.CanRead {
		_, _ = PollOnce(ctx, s) // лента появится сразу, а не через минуту
	}
	if r.Agent == nil {
		return
	}
	text := fmt.Sprintf("tg-agent: владелец отказал в доступе к чату «%s» (запрос %s). Не проси снова без новой просьбы владельца.", r.Title, r.ID)
	if r.Status == accessGranted {
		text = fmt.Sprintf("tg-agent: владелец открыл чат «%s» (запрос %s): alias %q, %s. "+
			"Обращайся к нему по alias, правила — в tg_list_chats.", r.Title, r.ID, s.Ref(r.Alias), rights(r.CanRead, r.CanSend))
	}
	if err := inbox.Send(ctx, *r.Agent, "tg-agent", text); err != nil && r.Agent.Session != "" {
		_ = wakeT3(ctx, s, r.Agent.Session, text) // сессию остановил T3 Code
	}
}

// WaitAccess — tg_wait_access.
func WaitAccess(ctx context.Context, s *config.Settings, id string, timeoutSec int) (*omap.Map, error) {
	if err := onlyLocal(s); err != nil {
		return nil, err
	}
	if loadAccess(accessPath(s))[id] == nil {
		return nil, &NotFound{Msg: "Запроса " + id + " нет."}
	}
	limit := s.ApprovalTimeoutSec
	if timeoutSec > 0 && timeoutSec < limit {
		limit = timeoutSec
	}
	deadline := time.Now().Add(time.Duration(limit) * time.Second)
	resolved := func() bool {
		r := loadAccess(accessPath(s))[id]
		return r == nil || r.Status != accessPending
	}
	updates := lock.New(s.UpdatesLockPath(), 500*time.Millisecond)
	if !tgc.InService() && ownsUpdates(s) && updates.Acquire(ctx) == nil {
		// службы нет — качаем апдейты бота сами, пока ждём
		Pump(ctx, s, deadline, resolved)
		updates.Release()
	} else {
		for time.Now().Before(deadline) && !resolved() && ctx.Err() == nil {
			sleepCtx(ctx, time.Second)
		}
	}
	r := loadAccess(accessPath(s))[id]
	if r == nil {
		return nil, errors.New("запрос пропал")
	}
	out := omap.New().Set("request_id", id).Set("chat_id", r.Peer).Set("title", r.Title).Set("status", r.Status)
	switch r.Status {
	case accessGranted:
		out.Set("alias", s.Ref(r.Alias)).Set("can_read", r.CanRead).Set("can_send", r.CanSend).
			Set("hint", "Чат в белом списке — обращайся к нему по alias.")
	case accessDenied:
		out.Set("hint", "Владелец отказал. Не проси снова без его новой просьбы.")
	default:
		out.Set("status", "waiting").Set("hint", "Владелец ещё не нажал кнопку. Это не отказ: решение придёт уведомлением; займись другим.")
	}
	return out, nil
}

// ── alias ────────────────────────────────────────────────────────────────

var translit = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e", 'ж': "zh", 'з': "z", 'и': "i",
	'й': "y", 'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t",
	'у': "u", 'ф': "f", 'х': "h", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "sch", 'ъ': "", 'ы': "y", 'ь': "",
	'э': "e", 'ю': "yu", 'я': "ya",
}

// slug — alias из названия чата: латиница, цифры и дефисы, до 40 символов.
func slug(title, kind string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		var part string
		switch {
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			part = string(r)
		case translit[r] != "" || r == 'ъ' || r == 'ь':
			part = translit[r]
		default:
			if b.Len() > 0 {
				dash = true
			}
			continue
		}
		if part == "" {
			continue
		}
		if dash {
			b.WriteByte('-')
			dash = false
		}
		b.WriteString(part)
	}
	out := b.String()
	if len(out) > 40 {
		out = strings.TrimRight(out[:40], "-")
	}
	if out == "" {
		out = "chat"
	}
	switch kind {
	case "group":
		return out + "-chat"
	case "channel":
		return out + "-channel"
	}
	return out
}

func uniqueAlias(s *config.Settings, base string) string {
	taken := func(a string) bool {
		for alias := range s.Chats {
			if strings.EqualFold(alias, a) {
				return true
			}
		}
		return false
	}
	alias := base
	for i := 2; taken(alias); i++ {
		alias = fmt.Sprintf("%s-%d", base, i)
	}
	return alias
}
