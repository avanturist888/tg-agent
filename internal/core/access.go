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
	"errors"
	"fmt"
	"html"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"tgagent/internal/bot"
	"tgagent/internal/chatsfile"
	"tgagent/internal/config"
	"tgagent/internal/inbox"
	"tgagent/internal/lock"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/store"
	"tgagent/internal/tgc"
)

const remoteNoFolders = "Папки и запросы доступа недоступны агенту из контейнера: его чаты задаёт " +
	"владелец в config/agents.toml. Нужен ещё чат — попроси владельца."

const (
	accessPending = "pending"
	accessGranted = "granted"
	accessDenied  = "denied"
	// accessStale — владелец разрешил, но выполнить уже нельзя: то, о чём
	// просили, изменилось, пока карточка висела (папка-ссылка не та). Запрос
	// закрыт — агент может спросить заново
	accessStale = "stale"
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
	if n := netOf(s); n != nil {
		return n.listFolders(ctx, s)
	}
	folders, byAgent, err := listedFolders(ctx, s, func() ([]tgc.Folder, error) {
		var folders []tgc.Folder
		err := tgc.Run(ctx, s, tgc.Opts{Caller: "folders"}, func(ctx context.Context, c *tgc.Conn) error {
			var err error
			folders, err = c.Folders(ctx)
			return err
		})
		return folders, err
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
			item.Set("shared", true).Set("shared_by_owner", f.MyInvites)
		}
		item.Set("pinned_and_included", len(f.Pinned)+len(f.Include))
		if len(f.Exclude) > 0 {
			item.Set("hidden", len(f.Exclude))
		}
		if byAgent[f.ID] {
			item.Set("made_by_agent", true)
		}
		if rules := f.Rules(); len(rules) > 0 {
			item.Set("rules", rules)
		}
		if set, ok := folderSet(s, strconv.Itoa(f.ID), f.Title); ok {
			item.Set("access", omap.New().Set("can_read", set.Read).Set("can_send", set.Send).Set("auto_send", set.AutoSend()))
		}
		list = append(list, item)
	}
	logEvent(s, "folders_listed", "count", len(folders))
	return omap.New().Set("folders", list).Set("hint",
		"Чаты папки — tg_folder_chats(folder=<id или название>). access — папка открыта агентам целиком; "+
			"нужна вся папка — tg_request_access(folder=…)."), nil
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
	if n := netOf(s); n != nil {
		return n.folderChats(ctx, s, folder)
	}
	var (
		found tgc.Folder
		rows  []tgc.DialogRow
		known = map[int64]config.ChatRule{} // id → правило белого списка
	)
	err := watchFolders(ctx, s, func(see func([]tgc.Folder) *agentBook) error {
		return tgc.Run(ctx, s, tgc.Opts{Wait: 30 * time.Second, Caller: "folder_chats"}, func(ctx context.Context, c *tgc.Conn) error {
			folders, err := c.Folders(ctx)
			if err != nil {
				return err
			}
			see(folders)
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
	})
	if err != nil {
		return nil, err
	}
	pending := map[int64]string{}
	for _, r := range loadAccess(s) {
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
	ID string `json:"id"`
	// Scope — о чём просят: "" — один чат, иначе scopeFolder, scopePublic,
	// scopeManage или scopeLeave (покинуть чужую папку-ссылку)
	Scope  string   `json:"scope,omitempty"`
	Folder string   `json:"folder,omitempty"` // id папки (scopeFolder)
	Count  int      `json:"count,omitempty"`  // сколько чатов в группе на момент запроса
	Sample []string `json:"sample,omitempty"` // какие (для карточки)
	// Members — состав папки-ссылки на момент запроса (scopeLeave): по
	// нажатию папка должна быть той же, а не новой с тем же номером
	Members []int64 `json:"members,omitempty"`
	// Outcome — почему запрос закрыт без выполнения (accessStale)
	Outcome  string      `json:"outcome,omitempty"`
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
	CanAuto  bool        `json:"can_auto,omitempty"` // автоотправка — решил владелец при допуске
	Agent    *inbox.Addr `json:"agent,omitempty"`    // куда сообщить о решении
	CardID   int64       `json:"card_id,omitempty"`
	// кто сейчас отправляет карточку (запрос записан до неё — publishAccess);
	// умер, не отправив, — запрос сирота, его карточку шлёт следующий
	PubPID   int       `json:"pub_pid,omitempty"`
	PubSince float64   `json:"pub_since,omitempty"`
	Created  time.Time `json:"created"`
	Decided  time.Time `json:"decided,omitzero"`
	// Result — итог для карточки, записан вместе с решением. CardShown —
	// итог в карточке показан, AgentTold — агенту сказано (или некому),
	// Told — оба. Решено, а не доведено (процесс умер, Bot API или агент не
	// ответили) — доводят повтор нажатия и уборка (retellAccess), каждое —
	// только недоведённое
	Result    string `json:"result,omitempty"`
	CardShown bool   `json:"card_shown,omitempty"`
	// CardLost — карточку отправляли, но её номер неизвестен (Bot API не
	// ответил — она могла и уйти): итог тогда — отдельным сообщением
	CardLost  bool `json:"card_lost,omitempty"`
	AgentTold bool `json:"agent_told,omitempty"`
	Told      bool `json:"told,omitempty"`
}

// accessTab — запросы доступа по id.
var accessTab = store.NewTable[*accessRequest](store.TAccessRequests)

// loadAccess — все запросы (не прочитались — пусто: запросы только
// показываются и ищутся, решения по ним — через editAccess).
func loadAccess(s *config.Settings) map[string]*accessRequest {
	m, err := loadAccessErr(s)
	if err != nil {
		logEvent(s, "access_load_failed", "error", err.Error())
		return map[string]*accessRequest{}
	}
	return m
}

// saveAccessDecision — запись решения владельца (тесты подменяют сбоем).
var saveAccessDecision = editAccess

// loadAccessErr — запросы доступа; сбой базы — ошибкой, а не «запросов нет».
func loadAccessErr(s *config.Settings) (map[string]*accessRequest, error) {
	db, err := stateDB(s)
	if err != nil {
		return nil, err
	}
	return accessTab.All(context.Background(), db.Q())
}

// editAccess — прочитать, поменять и записать запросы одной транзакцией
// (между процессами тоже: запрос может прийти из tg tool-call без службы).
func editAccess(s *config.Settings, fn func(m map[string]*accessRequest) bool) error {
	db, err := stateDB(s)
	if err != nil {
		return err
	}
	return accessTab.Edit(context.Background(), db, fn)
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
	if chatID == 0 && !s.IsMAX() { // в MAX 0 — «Избранное»
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
	var err error
	if n := netOf(s); n != nil {
		if info, err = n.chatInfo(ctx, chatID); err != nil {
			return nil, err
		}
		for _, r := range s.Rules() {
			if id, e := n.ruleChatID(r); e == nil && id == chatID {
				existing = &r
				break
			}
		}
	} else {
		err = tgc.Run(ctx, s, tgc.Opts{Wait: 30 * time.Second, Caller: "request_access"}, func(ctx context.Context, c *tgc.Conn) error {
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
	}
	if err != nil {
		return nil, err
	}
	out := omap.New().Set("chat_id", chatID).Set("title", info.Title)
	if existing != nil && (existing.Read || !read) && (existing.Send || !send) {
		return out.Set("status", "already_allowed").Set("alias", s.Ref(existing.Alias)).
			Set("can_read", existing.Read).Set("can_send", existing.Send), nil
	}
	// такой же запрос ещё ждёт кнопки — вторую карточку не шлём
	for _, r := range loadAccess(s) {
		if r.Status == accessPending && r.Peer == chatID && (r.Read || !read) && (r.Send || !send) {
			if _, err := adoptOrphan(ctx, s, r.ID); err != nil { // карточка того запроса не ушла — отправить
				return nil, err
			}
			return out.Set("request_id", r.ID).Set("status", accessPending).Set("next_step",
				"Такой запрос уже ждёт владельца. Дождись: tg_wait_access(request_id)."), nil
		}
	}
	req := &accessRequest{ID: outbox.NewID(), Peer: chatID, Title: info.Title, Kind: info.Kind, Username: info.Username,
		Read: read, Send: send, Reason: reason, Status: accessPending, Agent: agentFrom(ctx), Created: time.Now()}
	if existing != nil {
		req.Alias = existing.Alias
	}
	sent, err := publishAccess(ctx, s, req, existing)
	if err != nil {
		return nil, err
	}
	logEvent(s, "access_requested", "request_id", req.ID, "chat_id", chatID, "title", info.Title,
		"read", read, "send", send, "reason", reason)
	out.Set("request_id", req.ID).Set("status", accessPending).Set("asked", rights(read, send))
	if !sent {
		return out.Set("next_step", cardPendingStep), nil
	}
	return out.Set("next_step", "Карточка ушла владельцу в бот. Когда он нажмёт кнопку, сюда придёт уведомление "+
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
	if r.Scope != "" {
		return setCardMarkdown(r, status)
	}
	var b strings.Builder
	b.WriteString("🔑 **Агент просит доступ к чату**\n\n")
	chat := "«" + bot.MDEscape(r.Title) + "» — " + kindRu(r.Kind)
	if r.Username != "" {
		chat += ", @" + bot.MDEscape(r.Username)
	}
	fmt.Fprintf(&b, "**Чат:** %s\n", chat)
	fmt.Fprintf(&b, "**Просит:** %s\n", rights(r.Read, r.Send))
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
	p := s.CallbackPrefix() + "a:" + r.ID + ":"
	var rows [][]map[string]string
	if r.Scope == scopePublic || r.Scope == scopeManage || r.Scope == scopeLeave {
		rows = append(rows, []map[string]string{{"text": "✅ Разрешить", "callback_data": p + "r"}})
	} else if r.Send {
		// автоотправку владелец решает здесь же, при допуске: в допущенном чате агент действует сам
		rows = append(rows,
			[]map[string]string{{"text": "⚡ Разрешить, отправка без подтверждения", "callback_data": p + "rwa"}},
			[]map[string]string{{"text": "✅ Разрешить, каждое сообщение — через карточку", "callback_data": p + "rw"}})
	} else {
		rows = append(rows, []map[string]string{{"text": "✅ Разрешить", "callback_data": p + "r"}})
	}
	if r.Send && r.Read {
		rows = append(rows, []map[string]string{{"text": "👁 Только чтение", "callback_data": p + "r"}})
	}
	rows = append(rows, []map[string]string{{"text": "✋ Отказать", "callback_data": p + "no"}})
	return map[string]any{"inline_keyboard": rows}
}

// publishAccess — записать запрос и отправить владельцу карточку. Id
// запроса закрепляется в базе до карточки: он уходит в её кнопки, и
// совпадение со старым запросом после этого направило бы нажатие не туда.
// Пока карточка не ушла, запрос помечен отправляющим процессом (PubPID):
// умрёт он между записью и карточкой — запрос не повиснет без кнопок
// (orphaned, publishOrphanAccess). Карточка не ушла (или ответ Bot API
// потерялся — она могла и уйти) — запрос не удаляется: он становится
// сиротой, карточку пришлёт уборка, а нажатие по уже ушедшей найдёт его.
// false — карточка пока не ушла.
func publishAccess(ctx context.Context, s *config.Settings, req *accessRequest, existing *config.ChatRule) (bool, error) {
	req.PubPID, req.PubSince = os.Getpid(), pubNow()
	if err := editAccess(s, func(m map[string]*accessRequest) bool {
		for m[req.ID] != nil { // id случайный: не занять чужой
			req.ID = outbox.NewID()
		}
		m[req.ID] = req
		return true
	}); err != nil {
		return false, err
	}
	card, err := sendAccessCard(ctx, s, req, existing)
	if err != nil {
		logEvent(s, "access_card_failed", "request_id", req.ID, "error", err.Error())
		_ = editAccess(s, func(m map[string]*accessRequest) bool {
			if r := m[req.ID]; r != nil && r.CardID == 0 && r.PubPID == req.PubPID {
				r.PubPID, r.PubSince = 0, 0 // сирота: карточку пришлёт уборка
				r.CardLost = true
				return true
			}
			return false
		})
		return false, nil
	}
	return true, recordAccessCard(s, req, card)
}

// cardPendingStep — next_step агенту, если карточка запроса ещё не ушла.
const cardPendingStep = "Запрос записан, но карточка владельцу в бот пока не ушла (сбой Bot API) — служба " +
	"пришлёт её сама. Дождаться решения — tg_wait_access(request_id). Не проси повторно."

func pubNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// recordAccessCard — карточка ушла: запомнить её и снять отметку отправки.
// Владелец успел решить по ней раньше, чем её номер записался, — итог в
// неё ещё не показан (показывать было некуда): CardShown снимается, его
// доведут повтор нажатия и уборка.
func recordAccessCard(s *config.Settings, req *accessRequest, card int64) error {
	req.CardID, req.PubPID, req.PubSince = card, 0, 0
	return editAccess(s, func(m map[string]*accessRequest) bool {
		if r := m[req.ID]; r != nil {
			r.CardID, r.PubPID, r.PubSince, r.CardLost = card, 0, 0, false
			if r.Status != accessPending && r.Result != "" {
				r.CardShown, r.Told = false, false
			}
			return true
		}
		return false
	})
}

// orphaned — запрос ждёт кнопки, а карточки нет и отправлять её некому:
// отправлявший процесс умер между записью запроса и карточкой.
func (r *accessRequest) orphaned() bool {
	return r.Status == accessPending && r.CardID == 0 && (r.PubPID == 0 || !lock.PIDAlive(r.PubPID, r.PubSince))
}

// adoptOrphan — отправить карточку осиротевшего запроса (тот же id: его
// уже знает агент). Взять его можно только одному — отметка ставится в той
// же транзакции, где проверено, что он сирота. false — не сирота (карточка
// есть или её отправляет живой процесс).
func adoptOrphan(ctx context.Context, s *config.Settings, id string) (bool, error) {
	var req *accessRequest
	if err := editAccess(s, func(m map[string]*accessRequest) bool {
		r := m[id]
		if r == nil || !r.orphaned() {
			return false
		}
		r.PubPID, r.PubSince = os.Getpid(), pubNow()
		cp := *r
		req = &cp
		return true
	}); err != nil || req == nil {
		return false, err
	}
	var existing *config.ChatRule
	if rule, ok := s.Chats[req.Alias]; ok && req.Alias != "" && req.Scope == "" {
		existing = &rule
	}
	card, err := sendAccessCard(ctx, s, req, existing)
	if err != nil {
		// отметку снять: следующая уборка попробует снова (запрос оставляем —
		// агент уже ждёт его id)
		_ = editAccess(s, func(m map[string]*accessRequest) bool {
			if r := m[id]; r != nil && r.CardID == 0 {
				r.PubPID, r.PubSince = 0, 0
				r.CardLost = true
				return true
			}
			return false
		})
		logEvent(s, "access_card_failed", "request_id", id, "error", err.Error())
		return true, fmt.Errorf("карточка в бот не ушла: %w", err)
	}
	logEvent(s, "access_card_recovered", "request_id", id)
	return true, recordAccessCard(s, req, card)
}

// publishOrphanAccess — уборка службы: карточки осиротевших запросов.
func publishOrphanAccess(ctx context.Context, s *config.Settings) {
	for id, r := range loadAccess(s) {
		if r.orphaned() {
			_, _ = adoptOrphan(ctx, s, id)
		}
	}
}

var sendAccessCard = postAccessCard // тесты подменяют

func postAccessCard(ctx context.Context, s *config.Settings, r *accessRequest, existing *config.ChatRule) (int64, error) {
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

// finishAccessCard — итог в карточке, кнопки снимаются; true — итог
// показан (или карточки нет и её никто не шлёт). Номера ещё нет, а
// карточку шлёт живой процесс — не показан: она вот-вот появится.
var finishAccessCard = func(ctx context.Context, s *config.Settings, r *accessRequest, status string) bool {
	if r.CardID == 0 {
		if r.PubPID != 0 && lock.PIDAlive(r.PubPID, r.PubSince) {
			return false
		}
		if r.CardLost || r.PubPID != 0 {
			// карточка могла уйти, а её номера нет — итог отдельным
			// сообщением, иначе владелец его не увидит
			return sayErr(ctx, s, "Запрос доступа к «"+html.EscapeString(r.Title)+"»: "+
				html.EscapeString(stripMD(status))) == nil
		}
		return true
	}
	err := bot.Call(ctx, s, "editMessageText", map[string]any{
		"chat_id": s.ApprovalChatID, "message_id": r.CardID,
		"rich_message": map[string]string{"markdown": accessCardMarkdown(r, nil, status)},
	}, nil)
	if err != nil {
		logEvent(s, "card_edit_failed", "request_id", r.ID, "error", err.Error())
		_ = bot.Call(ctx, s, "editMessageReplyMarkup", map[string]any{"chat_id": s.ApprovalChatID, "message_id": r.CardID}, nil)
		return false
	}
	return true
}

// handleAccessCallback — нажатие под карточкой запроса доступа («a:<id>:<rwa|rw|r|no>»).
// Решение сначала записывается, потом владелец видит итог; сбой базы
// (чтение запроса, запись решения) — retryable: нажатие разберётся снова
// (выдача прав повторяется без вреда — то же правило в chats.toml).
// Отказ выдачи прав по существу — владельцу «Не вышло», кнопки остаются.
func handleAccessCallback(ctx context.Context, s *config.Settings, q *bot.CallbackQuery) error {
	parts := strings.SplitN(q.Data, ":", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	id, verdict := parts[1], parts[2]
	all, err := loadAccessErr(s)
	if err != nil {
		return retryable{err}
	}
	r := all[id]
	if r == nil {
		bot.AnswerCallback(ctx, s, q.ID, "Запрос не найден.")
		return nil
	}
	if r.Status != accessPending {
		bot.AnswerCallback(ctx, s, q.ID, "Уже решено.")
		if r.Result != "" && !r.Told {
			tellAccess(ctx, s, r) // решение записано, а итог не доведён
		}
		return nil
	}
	var status, reply string
	switch verdict {
	case "rwa", "rw", "r":
		read, send, auto := true, verdict != "r", verdict == "rwa"
		if !r.Read && send {
			read = false // просили только отправку
		}
		fresh, err := config.Load() // белый список свежий: его могли поправить, пока карточка висела
		if err != nil {
			return grantFailed(ctx, s, q, id, err)
		}
		if r.Scope != "" {
			if err := grantSet(ctx, fresh, r, read, send, auto); err != nil {
				var st *staleRequest
				if errors.As(err, &st) {
					// исполнять уже нечего — закрыть запрос, а не держать его
					// «ждущим»: иначе новый запрос агента упрётся в этот
					r.Status, r.Outcome = accessStale, st.msg
					status = "⌛ **Не выполнено:** " + bot.MDEscape(st.msg)
					reply = "Ничего не менял: " + st.msg
					break
				}
				return grantFailed(ctx, s, q, id, err)
			}
			r.Status, r.CanRead, r.CanSend, r.CanAuto = accessGranted, read, send, auto
			status = "✅ **Разрешено:** " + setGranted(r)
			reply = "Разрешено."
			break
		}
		alias, err := grantAccess(fresh, r, read, send, auto)
		if err != nil {
			return grantFailed(ctx, s, q, id, err)
		}
		r.Status, r.Alias, r.CanRead, r.CanSend, r.CanAuto = accessGranted, alias, read, send, auto
		status = fmt.Sprintf("✅ **Разрешено:** %s, alias `%s`", grantedRights(r), alias)
		reply = "Чат открыт агентам."
	default:
		r.Status = accessDenied
		status = "✋ **Отказано** — белый список не менялся."
		reply = "Отказано."
	}
	r.Decided = time.Now()
	r.Result = status
	// решение — в свежую строку: пока разбирали нажатие, публикация могла
	// записать номер карточки (recordAccessCard), и прежний снимок его бы
	// затёр. Номера нет, а нажатие пришло с карточки, — он из нажатия
	// (ответ Bot API на отправку мог потеряться)
	if err := saveAccessDecision(s, func(m map[string]*accessRequest) bool {
		cur := m[id]
		if cur == nil || cur.Status != accessPending {
			return false
		}
		cur.Status, cur.Outcome, cur.Alias = r.Status, r.Outcome, r.Alias
		cur.CanRead, cur.CanSend, cur.CanAuto = r.CanRead, r.CanSend, r.CanAuto
		cur.Decided, cur.Result = r.Decided, r.Result
		if cur.CardID == 0 && q.Message != nil {
			cur.CardID, cur.CardLost = q.Message.MessageID, false
		}
		cp := *cur
		r = &cp
		return true
	}); err != nil {
		logEvent(s, "access_save_failed", "request_id", id, "error", err.Error())
		return retryable{err}
	}
	bot.AnswerCallback(ctx, s, q.ID, reply)
	logEvent(s, "access_"+r.Status, "request_id", id, "scope", r.Scope, "chat_id", r.Peer, "title", r.Title, "alias", r.Alias,
		"read", r.CanRead, "send", r.CanSend, "auto_send", r.CanAuto, "by", "telegram_button")
	go func() {
		ctx := context.WithoutCancel(ctx)
		afterAccess(ctx, r) // ленты и группы — до сообщения агенту: он сразу пойдёт читать
		tellAccess(ctx, s, r)
	}()
	return nil
}

// tellAccess — довести записанное решение: то, что ещё не доведено, —
// итог в карточку (CardShown), сообщение агенту (AgentTold); отметки —
// только за вышедшее. Умер процесс до отметки — повтор нажатия или уборка
// (retellAccess) доведут снова (сообщение агенту может прийти дважды —
// лучше, чем ни разу).
func tellAccess(ctx context.Context, s *config.Settings, r *accessRequest) {
	card, agent := r.CardShown, r.AgentTold
	if !card {
		card = finishAccessCard(ctx, s, r, r.Result)
	}
	if !agent {
		agent = tellAgentAccess(ctx, r)
	}
	if err := editAccess(s, func(m map[string]*accessRequest) bool {
		cur := m[r.ID]
		if cur == nil || cur.Status != r.Status {
			return false
		}
		cur.CardShown = cur.CardShown || card
		cur.AgentTold = cur.AgentTold || agent
		cur.Told = cur.CardShown && cur.AgentTold
		return true
	}); err != nil {
		logEvent(s, "access_tell_failed", "request_id", r.ID, "error", err.Error())
	}
}

// retellGiveUp — дольше не доводим: карточку могли удалить, сессию агента —
// закрыть насовсем.
const retellGiveUp = 24 * time.Hour

// retellAccess — уборка: решения, записанные, но не доведённые. Свежие не
// трогаем — их сейчас доводит сам решивший (afterAccess ждёт до 2 минут).
func retellAccess(ctx context.Context, s *config.Settings) {
	for _, r := range loadAccess(s) {
		if ctx.Err() != nil {
			return
		}
		if r.Status == accessPending || r.Result == "" || r.Told || time.Since(r.Decided) < 3*time.Minute {
			continue
		}
		if time.Since(r.Decided) > retellGiveUp {
			logEvent(s, "access_tell_gave_up", "request_id", r.ID, "card_shown", r.CardShown, "agent_told", r.AgentTold)
			_ = editAccess(s, func(m map[string]*accessRequest) bool {
				if cur := m[r.ID]; cur != nil && !cur.Told {
					cur.Told = true
					return true
				}
				return false
			})
			continue
		}
		logEvent(s, "access_retold", "request_id", r.ID)
		tellAccess(ctx, s, r)
	}
}

// grantFailed — выдать права не вышло (конфиг не прочитался, chats.toml не
// записался, Telegram не ответил при выходе из папки): решение владельца
// не исполнено — повтор (retryable), а не «обработано». Не помогут повторы
// — владелец получит просьбу нажать ещё раз с этой ошибкой.
func grantFailed(ctx context.Context, s *config.Settings, q *bot.CallbackQuery, id string, err error) error {
	bot.AnswerCallback(ctx, s, q.ID, "Не вышло: "+err.Error()+" — повторяю.")
	logEvent(s, "access_grant_failed", "request_id", id, "error", err.Error())
	if isRetry(err) {
		return err
	}
	return retryable{err}
}

// grantedRights — права из решения владельца, для карточки и агента.
func grantedRights(r *accessRequest) string {
	switch {
	case r.CanSend && r.CanAuto:
		return rights(r.CanRead, true) + ", отправка без подтверждения"
	case r.CanSend:
		return rights(r.CanRead, true) + ", каждое сообщение — через карточку"
	}
	return rights(r.CanRead, false)
}

// grantAccess — дописать чат в белый список (или расширить права). Возвращает alias.
func grantAccess(s *config.Settings, r *accessRequest, read, send, auto bool) (string, error) {
	for _, rule := range s.Rules() {
		if rulePeerID(rule) == r.Peer && rule.Via != "" {
			// чат открыт через группу — свои права ему даёт отдельный [[chat]]
			// с тем же alias: явное правило главнее группы
			note := fmt.Sprintf("Открыт по запросу агента %s: %s", time.Now().Format("02.01.2006"), r.Reason)
			return rule.Alias, chatsfile.Add(chatsfile.Chat{Alias: rule.Alias, Peer: r.Peer, Title: r.Title,
				Read: rule.Read || read, Send: rule.Send || send, Auto: rule.AutoSend() || (send && auto), Note: note})
		}
		if rulePeerID(rule) == r.Peer {
			return rule.Alias, chatsfile.Update(rule.Alias, rule.Read || read, rule.Send || send, rule.AutoSend() || auto, nil, nil)
		}
	}
	alias := uniqueAlias(s, slug(r.Title, r.Kind))
	note := fmt.Sprintf("Открыт по запросу агента %s: %s", time.Now().Format("02.01.2006"), r.Reason)
	return alias, chatsfile.Add(chatsfile.Chat{Alias: alias, Peer: r.Peer, Title: r.Title, Read: read, Send: send, Auto: send && auto, Note: note})
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

// afterAccess — сразу завести ленту нового чата (и состав открытой группы),
// не дожидаясь сверки.
func afterAccess(ctx context.Context, r *accessRequest) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	s, err := config.Load()
	if err != nil {
		return
	}
	if r.Status == accessGranted && (r.Scope == scopeFolder || r.Scope == scopePublic) {
		if _, err := RefreshSets(ctx, s, true); err != nil {
			logEvent(s, "chat_sets_failed", "error", err.Error())
		}
		if s, err = config.Load(); err != nil {
			return
		}
	}
	if r.Status == accessGranted && r.CanRead && r.Scope != scopeManage && r.Scope != scopeLeave {
		if n := netOf(s); n != nil {
			n.catchup(ctx, s)
		} else {
			_, _ = PollOnce(ctx, s) // лента появится сразу, а не через минуту
		}
	}
}

// tellAgentAccess — сообщить агенту о решении; true — дошло (или некому).
var tellAgentAccess = func(ctx context.Context, r *accessRequest) bool {
	if r.Agent == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	s, err := config.Load()
	if err != nil {
		return false
	}
	text := fmt.Sprintf("tg-agent: владелец отказал в доступе к чату «%s» (запрос %s). Не проси снова без новой просьбы владельца.", r.Title, r.ID)
	if r.Scope != "" {
		text = setDecisionText(s, r)
	} else if r.Status == accessGranted {
		text = fmt.Sprintf("tg-agent: владелец открыл чат «%s» (запрос %s): alias %q, %s. "+
			"Обращайся к нему по alias, правила — в tg_list_chats.", r.Title, r.ID, s.Ref(r.Alias), grantedRights(r))
	}
	err = inbox.Send(ctx, *r.Agent, "tg-agent", text)
	if err != nil && r.Agent.Session != "" {
		err = wakeT3(ctx, s, r.Agent.Session, text) // сессию остановил T3 Code
	}
	return err == nil
}

// WaitAccess — tg_wait_access.
func WaitAccess(ctx context.Context, s *config.Settings, id string, timeoutSec int) (*omap.Map, error) {
	if err := onlyLocal(s); err != nil {
		return nil, err
	}
	if loadAccess(s)[id] == nil {
		return nil, &NotFound{Msg: "Запроса " + id + " нет."}
	}
	limit := s.ApprovalTimeoutSec
	if timeoutSec > 0 && timeoutSec < limit {
		limit = timeoutSec
	}
	deadline := time.Now().Add(time.Duration(limit) * time.Second)
	resolved := func() bool {
		r := loadAccess(s)[id]
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
	r := loadAccess(s)[id]
	if r == nil {
		return nil, errors.New("запрос пропал")
	}
	if r.Scope != "" {
		return setWaitResult(s, r), nil
	}
	out := omap.New().Set("request_id", id).Set("chat_id", r.Peer).Set("title", r.Title).Set("status", r.Status)
	switch r.Status {
	case accessGranted:
		out.Set("alias", s.Ref(r.Alias)).Set("can_read", r.CanRead).Set("can_send", r.CanSend).Set("auto_send", r.CanAuto).
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

// stripMD — итог карточки (markdown) простым текстом: без выделения и
// экранирования.
func stripMD(s string) string {
	s = strings.NewReplacer("**", "", "`", "").Replace(s)
	var b strings.Builder
	esc := false
	for _, r := range s {
		if r == '\\' && !esc {
			esc = true
			continue
		}
		esc = false
		b.WriteRune(r)
	}
	return b.String()
}
