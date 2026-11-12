package core

// Запросы доступа на группу чатов (папка, все публичные каналы) и на право
// раскладывать чаты по папкам. Карточка и кнопки — те же, что у запроса на
// один чат (callback «a:<id>:…»); решение пишется в chats.toml.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"tgagent/internal/bot"
	"tgagent/internal/chatsfile"
	"tgagent/internal/config"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/tgc"
)

const (
	scopeFolder = "folder"
	scopePublic = "public_channels"
	scopeManage = "manage_folders"
)

// SetAccessOpts — о чём просит агент, кроме одного чата.
type SetAccessOpts struct {
	Folder string // id или название папки
	Public bool   // все публичные каналы
	Manage bool   // раскладывать чаты по папкам и архиву
	Send   bool
	Reason string
}

// RequestSetAccess — tg_request_access с folder / public_channels / manage_folders.
func RequestSetAccess(ctx context.Context, s *config.Settings, o SetAccessOpts) (*omap.Map, error) {
	if err := onlyLocal(s); err != nil {
		return nil, err
	}
	o.Reason = strings.TrimSpace(o.Reason)
	if o.Reason == "" {
		return nil, &Bad{Msg: "reason обязателен: по нему владелец решает, давать ли доступ. Напиши, зачем и для какого проекта."}
	}
	if !s.BotReady() {
		return nil, &Bad{Msg: "Бот подтверждений не настроен — запросить доступ кнопкой нельзя. Попроси владельца: tg allow --help."}
	}
	req := &accessRequest{ID: outbox.NewID(), Read: true, Reason: o.Reason, Status: accessPending,
		Agent: agentFrom(ctx), Created: time.Now()}
	out := omap.New()
	switch {
	case o.Manage:
		if n := netOf(s); n != nil {
			return nil, &Bad{Msg: "В " + n.name() + " раскладывать чаты по папкам агент пока не умеет."}
		}
		req.Scope, req.Title = scopeManage, "папки и архив"
		if s.Manage.Folders {
			return out.Set("scope", scopeManage).Set("status", "already_allowed").
				Set("hint", "Право уже есть: tg_unfiled_chats, tg_move_chat, tg_archive_chat."), nil
		}
	case o.Public:
		if n := netOf(s); n != nil {
			return nil, &Bad{Msg: "В " + n.name() + " у каналов нет публичных @username — проси папку (folder)."}
		}
		req.Scope, req.Title = scopePublic, "публичные каналы"
		if set, ok := setOf(s, config.PublicChannelsKey); ok && set.Read {
			return out.Set("scope", scopePublic).Set("status", "already_allowed").
				Set("hint", "Публичные каналы уже открыты: они в tg_list_chats с via «публичные каналы»."), nil
		}
		err := tgc.Run(ctx, s, tgc.Opts{Wait: 30 * time.Second, Caller: "request_access"}, func(ctx context.Context, c *tgc.Conn) error {
			page, err := c.Dialogs(ctx)
			if err != nil {
				return err
			}
			sample(req, c.PublicChannels(page))
			return nil
		})
		if err != nil {
			return nil, err
		}
	default:
		folder := strings.TrimSpace(o.Folder)
		if folder == "" {
			return nil, &Bad{Msg: "Укажи chat_id, folder, public_channels или manage_folders."}
		}
		req.Scope, req.Send = scopeFolder, o.Send
		if err := folderSample(ctx, s, req, folder); err != nil {
			return nil, err
		}
		if set, ok := folderSet(s, req.Folder, req.Title); ok && set.Read && (set.Send || !o.Send) {
			return out.Set("scope", scopeFolder).Set("folder", req.Title).Set("status", "already_allowed").
				Set("can_send", set.Send).Set("hint", "Папка уже открыта: её чаты в tg_list_chats с via «"+set.Label()+"»."), nil
		}
	}
	for _, r := range loadAccess(accessPath(s)) {
		if r.Status == accessPending && r.Scope == req.Scope && r.Folder == req.Folder && (r.Send || !req.Send) {
			return out.Set("request_id", r.ID).Set("status", accessPending).Set("next_step",
				"Такой запрос уже ждёт владельца. Дождись: tg_wait_access(request_id)."), nil
		}
	}
	card, err := sendAccessCard(ctx, s, req, nil)
	if err != nil {
		return nil, fmt.Errorf("карточка в бот не ушла: %w", err)
	}
	req.CardID = card
	if err := editAccess(s, func(m map[string]*accessRequest) bool { m[req.ID] = req; return true }); err != nil {
		return nil, err
	}
	logEvent(s, "access_requested", "request_id", req.ID, "scope", req.Scope, "folder", req.Folder, "title", req.Title,
		"count", req.Count, "send", req.Send, "reason", req.Reason)
	return out.Set("request_id", req.ID).Set("scope", req.Scope).Set("status", accessPending).
		Set("next_step", "Карточка ушла владельцу в бот. Когда он нажмёт кнопку, сюда придёт уведомление "+
			"(если сессия жива); дождаться явно — tg_wait_access(request_id). Не проси повторно."), nil
}

func sample(r *accessRequest, rows []tgc.DialogRow) {
	r.Count = len(rows)
	for i, row := range rows {
		if i == 12 {
			break
		}
		r.Sample = append(r.Sample, row.Title)
	}
}

// folderSample — папка по id или названию: её id, название и чаты.
func folderSample(ctx context.Context, s *config.Settings, r *accessRequest, folder string) error {
	if n := netOf(s); n != nil {
		return n.folderSample(ctx, s, r, folder)
	}
	return tgFolderSample(ctx, s, r, folder)
}

// maxFolderSample — folderSample для MAX.
func maxFolderSample(ctx context.Context, s *config.Settings, r *accessRequest, folder string) error {
	{
		c, err := maxConn(ctx)
		if err != nil {
			return err
		}
		folders, err := c.Folders(ctx)
		if err != nil {
			return err
		}
		var names []string
		for _, f := range folders {
			names = append(names, fmt.Sprintf("%s «%s»", f.ID, f.Title))
			if f.ID == folder || strings.EqualFold(f.Title, folder) {
				r.Folder, r.Title = f.ID, f.Title
			}
		}
		if r.Folder == "" {
			return &Bad{Msg: fmt.Sprintf("Папки «%s» нет. Есть: %s.", folder, strings.Join(names, ", "))}
		}
		res, err := maxFolderChats(ctx, s, r.Folder)
		if err != nil {
			return err
		}
		if chats, ok := res.Get("chats"); ok {
			if list, ok := chats.([]*omap.Map); ok {
				r.Count = len(list)
				for i, m := range list {
					if i == 12 {
						break
					}
					t, _ := m.Get("title")
					r.Sample = append(r.Sample, fmt.Sprint(t))
				}
			}
		}
		return nil
	}
}

func tgFolderSample(ctx context.Context, s *config.Settings, r *accessRequest, folder string) error {
	return tgc.Run(ctx, s, tgc.Opts{Wait: 30 * time.Second, Caller: "request_access"}, func(ctx context.Context, c *tgc.Conn) error {
		folders, err := c.Folders(ctx)
		if err != nil {
			return err
		}
		f, ok := findTGFolder(folders, folder, "")
		if !ok {
			f, ok = findTGFolder(folders, "", folder)
		}
		if !ok {
			var names []string
			for _, f := range folders {
				names = append(names, fmt.Sprintf("%d «%s»", f.ID, f.Title))
			}
			return &Bad{Msg: fmt.Sprintf("Папки «%s» нет. Есть: %s.", folder, strings.Join(names, ", "))}
		}
		r.Folder, r.Title = fmt.Sprint(f.ID), f.Title
		page, err := c.Dialogs(ctx)
		if err != nil {
			return err
		}
		sample(r, c.FolderRows(page, f))
		return nil
	})
}

func setCardMarkdown(r *accessRequest, status string) string {
	var b strings.Builder
	switch r.Scope {
	case scopeManage:
		b.WriteString("🗂 **Агент просит право раскладывать чаты по папкам**\n\n")
		b.WriteString("Сможет: видеть чаты вне папок (только названия), класть чаты в папки и вынимать, " +
			"убирать в архив и доставать. Сообщений это не открывает; чат, который он сам положит в " +
			"открытую агентам папку, доступа ему не даст.\n")
	case scopePublic:
		b.WriteString("📢 **Агент просит доступ ко всем публичным каналам**\n\n")
		fmt.Fprintf(&b, "**Каналы:** с @username, на которые ты подписан (без закрытых по ссылке, без групп и личек) — сейчас %d", r.Count)
		if len(r.Sample) > 0 {
			b.WriteString(": " + bot.MDEscape(strings.Join(r.Sample, ", ")))
			if r.Count > len(r.Sample) {
				b.WriteString(", …")
			}
		}
		b.WriteString("\n**Просит:** только чтение; новые подписки откроются сами\n")
	default:
		fmt.Fprintf(&b, "📁 **Агент просит доступ к папке «%s»**\n\n", bot.MDEscape(r.Title))
		fmt.Fprintf(&b, "**Чатов сейчас:** %d", r.Count)
		if len(r.Sample) > 0 {
			b.WriteString(" — " + bot.MDEscape(strings.Join(r.Sample, ", ")))
			if r.Count > len(r.Sample) {
				b.WriteString(", …")
			}
		}
		fmt.Fprintf(&b, "\n**Просит:** %s; чаты, которые ты положишь в папку потом, откроются сами\n", rights(true, r.Send))
	}
	fmt.Fprintf(&b, "**Зачем:** %s\n", bot.MDEscape(r.Reason))
	if status != "" {
		b.WriteString("\n" + status)
	}
	return b.String()
}

// grantSet — записать решение владельца в chats.toml.
func grantSet(s *config.Settings, r *accessRequest, read, send, auto bool) error {
	note := fmt.Sprintf("Открыто по запросу агента %s: %s", time.Now().Format("02.01.2006"), r.Reason)
	switch r.Scope {
	case scopeManage:
		return chatsfile.SetManageFolders(true)
	case scopePublic:
		clearBlocked(config.PublicChannelsKey)
		return chatsfile.SetPublicChannels(true, note)
	}
	if set, ok := folderSet(s, r.Folder, r.Title); ok {
		read, send, auto = read || set.Read, send || set.Send, auto || set.AutoSend()
		note = set.Note
	}
	clearBlocked(config.FolderKey(r.Folder, ""))
	return chatsfile.SetFolder(chatsfile.Folder{ID: r.Folder, Name: r.Title, Read: read, Send: send, Auto: send && auto, Note: note})
}

func setGranted(r *accessRequest) string {
	switch r.Scope {
	case scopeManage:
		return "агент раскладывает чаты по папкам и архиву"
	case scopePublic:
		return "публичные каналы открыты на чтение"
	}
	return "папка «" + r.Title + "» — " + grantedRights(r)
}

func setDecisionText(s *config.Settings, r *accessRequest) string {
	if r.Status != accessGranted {
		return fmt.Sprintf("tg-agent: владелец отказал (%s, запрос %s). Не проси снова без новой просьбы владельца.", r.Title, r.ID)
	}
	switch r.Scope {
	case scopeManage:
		return fmt.Sprintf("tg-agent: владелец дал право раскладывать чаты (запрос %s): tg_unfiled_chats, tg_move_chat, tg_archive_chat.", r.ID)
	}
	n := 0
	for _, rule := range s.Rules() {
		if strings.Contains(rule.Via, r.Title) || (r.Scope == scopePublic && strings.Contains(rule.Via, "публичные каналы")) {
			n++
		}
	}
	return fmt.Sprintf("tg-agent: владелец разрешил (запрос %s): %s — %d чатов. Список — tg_list_chats(set=%q) "+
		"или active_since; новые чаты группы откроются сами.", r.ID, setGranted(r), n, setListRef(r))
}

func setListRef(r *accessRequest) string {
	if r.Scope == scopePublic {
		return "public_channels"
	}
	return r.Title
}

func setWaitResult(s *config.Settings, r *accessRequest) *omap.Map {
	out := omap.New().Set("request_id", r.ID).Set("scope", r.Scope).Set("title", r.Title).Set("status", r.Status)
	switch r.Status {
	case accessGranted:
		out.Set("granted", setGranted(r))
		if r.Scope == scopeManage {
			out.Set("hint", "tg_unfiled_chats, tg_move_chat, tg_archive_chat.")
		} else {
			out.Set("hint", "Чаты группы — в tg_list_chats (поле via).")
		}
	case accessDenied:
		out.Set("hint", "Владелец отказал. Не проси снова без его новой просьбы.")
	default:
		out.Set("status", "waiting").Set("hint", "Владелец ещё не нажал кнопку. Это не отказ: решение придёт уведомлением; займись другим.")
	}
	return out
}
