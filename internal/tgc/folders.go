package tgc

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// Folder — папка Telegram (dialog filter). Чаты в неё попадают явно
// (Pinned, Include) или по правилам (типы чатов минус исключения).
type Folder struct {
	ID       int
	Title    string
	Emoticon string
	Shared   bool // папка-ссылка (chatlist): только явные чаты, без правил

	Pinned, Include, Exclude []int64 // id чатов в формате Bot API

	Contacts, NonContacts, Groups, Broadcasts, Bots bool
	ExcludeMuted, ExcludeRead, ExcludeArchived      bool
}

// Rules — правила папки словами («контакты», «группы»…), пусто — только явные чаты.
func (f Folder) Rules() []string {
	var out []string
	for _, r := range []struct {
		on   bool
		name string
	}{
		{f.Contacts, "контакты"}, {f.NonContacts, "не контакты"}, {f.Groups, "группы"},
		{f.Broadcasts, "каналы"}, {f.Bots, "боты"},
		{f.ExcludeMuted, "без заглушённых"}, {f.ExcludeRead, "без прочитанных"},
		{f.ExcludeArchived, "без архива"},
	} {
		if r.on {
			out = append(out, r.name)
		}
	}
	return out
}

// Folders — папки аккаунта (без «Все чаты»).
func (c *Conn) Folders(ctx context.Context) ([]Folder, error) {
	res, err := c.API.MessagesGetDialogFilters(ctx)
	if err != nil {
		return nil, err
	}
	self := c.Peers.SelfID()
	ids := func(list []tg.InputPeerClass) []int64 {
		out := make([]int64, 0, len(list))
		for _, p := range list {
			if id := inputMarkedID(p, self); id != 0 {
				out = append(out, id)
			}
		}
		return out
	}
	var out []Folder
	for _, f := range res.Filters {
		switch v := f.(type) {
		case *tg.DialogFilter:
			out = append(out, Folder{
				ID: v.ID, Title: strings.TrimSpace(v.Title.Text), Emoticon: v.Emoticon,
				Pinned: ids(v.PinnedPeers), Include: ids(v.IncludePeers), Exclude: ids(v.ExcludePeers),
				Contacts: v.Contacts, NonContacts: v.NonContacts, Groups: v.Groups, Broadcasts: v.Broadcasts,
				Bots: v.Bots, ExcludeMuted: v.ExcludeMuted, ExcludeRead: v.ExcludeRead, ExcludeArchived: v.ExcludeArchived,
			})
		case *tg.DialogFilterChatlist:
			out = append(out, Folder{
				ID: v.ID, Title: strings.TrimSpace(v.Title.Text), Emoticon: v.Emoticon, Shared: true,
				Pinned: ids(v.PinnedPeers), Include: ids(v.IncludePeers),
			})
		}
	}
	return out, nil
}

func inputMarkedID(p tg.InputPeerClass, self int64) int64 {
	switch v := p.(type) {
	case *tg.InputPeerSelf:
		return self
	case *tg.InputPeerUser:
		return v.UserID
	case *tg.InputPeerUserFromMessage:
		return v.UserID
	case *tg.InputPeerChat:
		return MarkedPeerID(&tg.PeerChat{ChatID: v.ChatID})
	case *tg.InputPeerChannel:
		return MarkedPeerID(&tg.PeerChannel{ChannelID: v.ChannelID})
	case *tg.InputPeerChannelFromMessage:
		return MarkedPeerID(&tg.PeerChannel{ChannelID: v.ChannelID})
	}
	return 0
}

// dialogFacts — то, по чему правила папки решают, входит ли чат.
type dialogFacts struct {
	id                          int64
	contact, bot, self          bool
	group, broadcast            bool
	muted, read, archived, user bool
}

// Contains — входит ли чат в папку (логика клиентов Telegram).
func (f Folder) contains(d dialogFacts) bool {
	for _, list := range [][]int64{f.Pinned, f.Include} {
		for _, id := range list {
			if id == d.id {
				return true
			}
		}
	}
	if f.Shared {
		return false
	}
	for _, id := range f.Exclude {
		if id == d.id {
			return false
		}
	}
	if f.ExcludeMuted && d.muted || f.ExcludeRead && d.read || f.ExcludeArchived && d.archived {
		return false
	}
	switch {
	case d.user && d.bot:
		return f.Bots
	case d.user:
		if d.contact || d.self {
			return f.Contacts
		}
		return f.NonContacts
	case d.broadcast:
		return f.Broadcasts
	case d.group:
		return f.Groups
	}
	return false
}

func facts(d Dialog, page *DialogPage, self int64) dialogFacts {
	out := dialogFacts{id: MarkedPeerID(d.Peer), muted: d.Muted, read: d.Unread == 0 && !d.UnreadMark,
		archived: d.FolderID == 1}
	switch p := d.Peer.(type) {
	case *tg.PeerUser:
		out.user, out.self = true, p.UserID == self
		if u, ok := page.Users[p.UserID]; ok {
			out.contact, out.bot = u.Contact, u.Bot
		}
	case *tg.PeerChat:
		out.group = true
	case *tg.PeerChannel:
		out.group = true
		if ch, ok := page.Chats[p.ChannelID].(*tg.Channel); ok && ch.Broadcast {
			out.group, out.broadcast = false, true
		}
	}
	return out
}

// FolderChats — чаты папки: сначала закреплённые, дальше в порядке списка
// чатов. limit — сколько диалогов просмотреть (0 — все).
func (c *Conn) FolderChats(ctx context.Context, f Folder, limit int) ([]DialogRow, error) {
	page, err := c.dialogsFresh(ctx, limit)
	if err != nil {
		return nil, err
	}
	self := c.Peers.SelfID()
	byID := map[int64]DialogRow{}
	var order []int64
	for _, d := range page.Dialogs {
		if !f.contains(facts(d, page, self)) {
			continue
		}
		row := dialogRow(d, page)
		byID[row.ID] = row
		order = append(order, row.ID)
	}
	var out []DialogRow
	used := map[int64]bool{}
	for _, id := range append(append([]int64{}, f.Pinned...), order...) {
		row, ok := byID[id]
		if !ok || used[id] {
			continue
		}
		used[id] = true
		out = append(out, row)
	}
	return out, nil
}

// ChatInfo — название и тип чата аккаунта по id (формат Bot API) — из
// Telegram, а не со слов агента: по нему владелец решает, давать ли доступ.
func (c *Conn) ChatInfo(ctx context.Context, marked int64) (DialogRow, error) {
	t, err := c.resolveID(ctx, marked)
	if err != nil {
		return DialogRow{}, err
	}
	row := DialogRow{ID: marked}
	switch t.Kind {
	case "self":
		row.Kind, row.Title = "user", "Избранное"
	case "user":
		in, ok := t.Input.(*tg.InputPeerUser)
		if !ok {
			return row, fmt.Errorf("чат %d: неожиданный тип", marked)
		}
		users, err := c.API.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUser{UserID: in.UserID, AccessHash: in.AccessHash}})
		if err != nil {
			return row, err
		}
		for _, u := range users {
			if user, ok := u.(*tg.User); ok {
				row.Kind, row.Username = "user", user.Username
				row.Title = strings.TrimSpace(strings.Join(nonEmpty(user.FirstName, user.LastName), " "))
				if user.Bot {
					row.Kind = "bot"
				}
			}
		}
	case "chat":
		res, err := c.API.MessagesGetChats(ctx, []int64{t.ID})
		if err != nil {
			return row, err
		}
		for _, ch := range res.GetChats() {
			row.Kind = "group"
			row.Title, _ = chatTitle(ch)
		}
	case "channel":
		in, ok := t.Input.(*tg.InputPeerChannel)
		if !ok {
			return row, fmt.Errorf("чат %d: неожиданный тип", marked)
		}
		res, err := c.API.ChannelsGetChannels(ctx, []tg.InputChannelClass{&tg.InputChannel{ChannelID: in.ChannelID, AccessHash: in.AccessHash}})
		if err != nil {
			return row, err
		}
		for _, ch := range res.GetChats() {
			row.Kind = "group"
			row.Title, row.Username = chatTitle(ch)
			if channel, ok := ch.(*tg.Channel); ok && channel.Broadcast {
				row.Kind = "channel"
			}
		}
	}
	if row.Kind == "" {
		return row, fmt.Errorf("чат %d не найден", marked)
	}
	return row, nil
}

// Полный обход диалогов — десятки запросов, и после пары повторов подряд
// Telegram отвечает FLOOD_WAIT. Агент листает папки одну за другой, поэтому
// список держим минуту, а короткий FLOOD_WAIT пережидаем.
const dialogsTTL = time.Minute

var dialogsCache struct {
	sync.Mutex
	at    time.Time
	limit int
	page  *DialogPage
}

func (c *Conn) dialogsFresh(ctx context.Context, limit int) (*DialogPage, error) {
	dialogsCache.Lock()
	defer dialogsCache.Unlock()
	if dialogsCache.page != nil && dialogsCache.limit == limit && time.Since(dialogsCache.at) < dialogsTTL {
		return dialogsCache.page, nil
	}
	for attempt := 0; ; attempt++ {
		page, err := c.IterDialogs(ctx, limit)
		if err == nil {
			dialogsCache.page, dialogsCache.at, dialogsCache.limit = page, time.Now(), limit
			return page, nil
		}
		wait, flood := tgerr.AsFloodWait(err)
		if !flood || wait > 30*time.Second || attempt == 2 {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait + time.Second):
		}
	}
}
