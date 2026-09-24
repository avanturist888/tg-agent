package tgc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"tgagent/internal/config"
	"tgagent/internal/lock"
)

// PeerCache — access_hash пользователей и каналов (data/peers.json).
//
// Без access_hash Telegram не даёт обратиться к пользователю или каналу по
// id, поэтому храним их отдельным файлом рядом с сессией.
type PeerCache struct {
	path string
	mu   sync.Mutex
	data peerData
	// dirty — есть несохранённые изменения
	dirty bool
}

type peerData struct {
	Self      int64             `json:"self,omitempty"`
	Users     map[string]int64  `json:"users"`
	Channels  map[string]int64  `json:"channels"`
	Usernames map[string]string `json:"usernames"` // username (lower) → "user:ID" / "channel:ID"
}

func LoadPeers(path string) *PeerCache {
	c := &PeerCache{path: path, data: peerData{
		Users: map[string]int64{}, Channels: map[string]int64{}, Usernames: map[string]string{},
	}}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &c.data)
	}
	if c.data.Users == nil {
		c.data.Users = map[string]int64{}
	}
	if c.data.Channels == nil {
		c.data.Channels = map[string]int64{}
	}
	if c.data.Usernames == nil {
		c.data.Usernames = map[string]string{}
	}
	return c
}

// Save пишет кэш на диск, если он менялся (слияние с тем, что записали другие).
func (c *PeerCache) Save() {
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return
	}
	mine := c.data
	c.dirty = false
	c.mu.Unlock()
	_ = lock.With(context.Background(), c.path, 10*time.Second, func() error {
		merged := LoadPeers(c.path).data
		for k, v := range mine.Users {
			merged.Users[k] = v
		}
		for k, v := range mine.Channels {
			merged.Channels[k] = v
		}
		for k, v := range mine.Usernames {
			merged.Usernames[k] = v
		}
		if mine.Self != 0 {
			merged.Self = mine.Self
		}
		raw, err := json.MarshalIndent(merged, "", " ")
		if err != nil {
			return err
		}
		tmp := c.path + ".tmp"
		if err := os.WriteFile(tmp, raw, 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, c.path)
	})
}

// Remember — запомнить пользователей и чаты из любого ответа Telegram.
func (c *PeerCache) Remember(users []tg.UserClass, chats []tg.ChatClass) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, u := range users {
		user, ok := u.(*tg.User)
		if !ok {
			continue
		}
		key := strconv.FormatInt(user.ID, 10)
		if hash, ok := user.GetAccessHash(); ok && !user.Min {
			if c.data.Users[key] != hash {
				c.data.Users[key] = hash
				c.dirty = true
			}
		}
		if user.Self && c.data.Self != user.ID {
			c.data.Self = user.ID
			c.dirty = true
		}
		if name, ok := user.GetUsername(); ok && name != "" {
			ref := "user:" + key
			if c.data.Usernames[strings.ToLower(name)] != ref {
				c.data.Usernames[strings.ToLower(name)] = ref
				c.dirty = true
			}
		}
	}
	for _, ch := range chats {
		channel, ok := ch.(*tg.Channel)
		if !ok {
			continue
		}
		key := strconv.FormatInt(channel.ID, 10)
		if hash, ok := channel.GetAccessHash(); ok && !channel.Min {
			if c.data.Channels[key] != hash {
				c.data.Channels[key] = hash
				c.dirty = true
			}
		}
		if name, ok := channel.GetUsername(); ok && name != "" {
			ref := "channel:" + key
			if c.data.Usernames[strings.ToLower(name)] != ref {
				c.data.Usernames[strings.ToLower(name)] = ref
				c.dirty = true
			}
		}
	}
}

func (c *PeerCache) user(id int64) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.data.Users[strconv.FormatInt(id, 10)]
	return h, ok
}

func (c *PeerCache) channel(id int64) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.data.Channels[strconv.FormatInt(id, 10)]
	return h, ok
}

func (c *PeerCache) byUsername(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data.Usernames[strings.ToLower(name)]
}

// SelfID — id владельца (из кэша).
func (c *PeerCache) SelfID() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data.Self
}

const channelShift = 1000000000000

// Target — разрешённый чат: как к нему обращаться и как он выглядит в ответах.
type Target struct {
	Input tg.InputPeerClass
	// Kind: "self", "user", "chat", "channel"
	Kind string
	// ID — «сырой» id (без -100); у self — id владельца
	ID int64
}

// MarkedID — id в формате Bot API: -100… для каналов, -… для групп.
func (t Target) MarkedID() int64 {
	switch t.Kind {
	case "chat":
		return -t.ID
	case "channel":
		return -(channelShift + t.ID)
	}
	return t.ID
}

// Self — id владельца (запрашивает у Telegram, если в кэше нет).
func (c *Conn) Self(ctx context.Context) (*tg.User, error) {
	users, err := c.API.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUserSelf{}})
	if err != nil {
		return nil, err
	}
	c.Peers.Remember(users, nil)
	for _, u := range users {
		if user, ok := u.(*tg.User); ok {
			return user, nil
		}
	}
	return nil, errors.New("Telegram не вернул текущего пользователя")
}

// Resolve — чат из белого списка → InputPeer.
func (c *Conn) Resolve(ctx context.Context, rule config.ChatRule) (Target, error) {
	switch p := rule.Peer.(type) {
	case int64:
		return c.resolveID(ctx, p)
	case string:
		s := strings.TrimSpace(p)
		if strings.EqualFold(s, "me") || strings.EqualFold(s, "self") {
			id := c.Peers.SelfID()
			if id == 0 {
				me, err := c.Self(ctx)
				if err != nil {
					return Target{}, err
				}
				id = me.ID
			}
			return Target{Input: &tg.InputPeerSelf{}, Kind: "self", ID: id}, nil
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return c.resolveID(ctx, n)
		}
		return c.resolveUsername(ctx, strings.TrimPrefix(s, "@"))
	}
	return Target{}, fmt.Errorf("непонятный id чата: %v", rule.Peer)
}

func (c *Conn) resolveUsername(ctx context.Context, name string) (Target, error) {
	if t, ok := c.fromRef(c.Peers.byUsername(name)); ok {
		return t, nil
	}
	res, err := c.API.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: name})
	if err != nil {
		return Target{}, err
	}
	c.Peers.Remember(res.Users, res.Chats)
	defer c.Peers.Save()
	switch p := res.Peer.(type) {
	case *tg.PeerUser:
		return c.resolveID(ctx, p.UserID)
	case *tg.PeerChannel:
		return c.resolveID(ctx, -(channelShift + p.ChannelID))
	case *tg.PeerChat:
		return c.resolveID(ctx, -p.ChatID)
	}
	return Target{}, fmt.Errorf("@%s: не удалось определить чат", name)
}

func (c *Conn) fromRef(ref string) (Target, bool) {
	kind, idStr, ok := strings.Cut(ref, ":")
	if !ok {
		return Target{}, false
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return Target{}, false
	}
	switch kind {
	case "user":
		if h, ok := c.Peers.user(id); ok {
			return Target{Input: &tg.InputPeerUser{UserID: id, AccessHash: h}, Kind: "user", ID: id}, true
		}
	case "channel":
		if h, ok := c.Peers.channel(id); ok {
			return Target{Input: &tg.InputPeerChannel{ChannelID: id, AccessHash: h}, Kind: "channel", ID: id}, true
		}
	}
	return Target{}, false
}

func (c *Conn) resolveID(ctx context.Context, marked int64) (Target, error) {
	lookup := func() (Target, bool) {
		switch {
		case marked > 0:
			if marked == c.Peers.SelfID() {
				return Target{Input: &tg.InputPeerSelf{}, Kind: "self", ID: marked}, true
			}
			return c.fromRef("user:" + strconv.FormatInt(marked, 10))
		case marked <= -channelShift:
			return c.fromRef("channel:" + strconv.FormatInt(-marked-channelShift, 10))
		default:
			return Target{Input: &tg.InputPeerChat{ChatID: -marked}, Kind: "chat", ID: -marked}, true
		}
	}
	if t, ok := lookup(); ok {
		return t, nil
	}
	// id есть, но его нет в локальном кэше — прогреваем диалогами
	if err := c.WarmDialogs(ctx, 0); err != nil {
		return Target{}, err
	}
	if t, ok := lookup(); ok {
		return t, nil
	}
	return Target{}, fmt.Errorf("чат %d не найден среди диалогов аккаунта", marked)
}

// Dialog — строка списка диалогов.
type Dialog struct {
	Peer     tg.PeerClass
	Unread   int
	Top      int
	FolderID int
}

// DialogPage — диалоги вместе с сущностями и последними сообщениями.
type DialogPage struct {
	Dialogs  []Dialog
	Users    map[int64]*tg.User
	Chats    map[int64]tg.ChatClass
	Messages map[string]tg.MessageClass // ключ — peerKey + ":" + id
}

// IterDialogs — все диалоги аккаунта (вместе с архивом), страницами по 100.
// limit <= 0 — без ограничения.
func (c *Conn) IterDialogs(ctx context.Context, limit int) (*DialogPage, error) {
	page := &DialogPage{Users: map[int64]*tg.User{}, Chats: map[int64]tg.ChatClass{}, Messages: map[string]tg.MessageClass{}}
	var (
		offsetDate int
		offsetID   int
		offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}
		seen                         = map[string]bool{}
	)
	for {
		want := 100
		if limit > 0 && limit-len(page.Dialogs) < want {
			want = limit - len(page.Dialogs)
		}
		if want <= 0 {
			break
		}
		res, err := c.API.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetDate: offsetDate, OffsetID: offsetID, OffsetPeer: offsetPeer, Limit: want,
		})
		if err != nil {
			return nil, err
		}
		var (
			dialogs  []tg.DialogClass
			messages []tg.MessageClass
			users    []tg.UserClass
			chats    []tg.ChatClass
			total    = -1
		)
		switch r := res.(type) {
		case *tg.MessagesDialogs:
			dialogs, messages, users, chats = r.Dialogs, r.Messages, r.Users, r.Chats
		case *tg.MessagesDialogsSlice:
			dialogs, messages, users, chats, total = r.Dialogs, r.Messages, r.Users, r.Chats, r.Count
		case *tg.MessagesDialogsNotModified:
			return page, nil
		}
		c.Peers.Remember(users, chats)
		for _, u := range users {
			if user, ok := u.(*tg.User); ok {
				page.Users[user.ID] = user
			}
		}
		for _, ch := range chats {
			page.Chats[ch.GetID()] = ch
		}
		for _, m := range messages {
			page.Messages[msgKey(m)] = m
		}
		added := 0
		for _, d := range dialogs {
			dlg, ok := d.(*tg.Dialog)
			if !ok {
				continue
			}
			key := peerKey(dlg.Peer)
			if seen[key] {
				continue
			}
			seen[key] = true
			folder, _ := dlg.GetFolderID()
			page.Dialogs = append(page.Dialogs, Dialog{Peer: dlg.Peer, Unread: dlg.UnreadCount, Top: dlg.TopMessage, FolderID: folder})
			added++
		}
		if len(dialogs) < want || added == 0 || (total >= 0 && len(page.Dialogs) >= total) {
			break
		}
		// следующая страница — от последнего диалога
		last := dialogs[len(dialogs)-1].(*tg.Dialog)
		offsetID = last.TopMessage
		if m, ok := page.Messages[peerKey(last.Peer)+":"+strconv.Itoa(last.TopMessage)]; ok {
			offsetDate = messageDate(m)
		}
		offsetPeer = c.inputPeerOf(last.Peer)
	}
	c.Peers.Save()
	return page, nil
}

// WarmDialogs — пройти диалоги, чтобы запомнить access_hash (limit 0 — все).
func (c *Conn) WarmDialogs(ctx context.Context, limit int) error {
	_, err := c.IterDialogs(ctx, limit)
	return err
}

func (c *Conn) inputPeerOf(p tg.PeerClass) tg.InputPeerClass {
	switch v := p.(type) {
	case *tg.PeerUser:
		if h, ok := c.Peers.user(v.UserID); ok {
			return &tg.InputPeerUser{UserID: v.UserID, AccessHash: h}
		}
	case *tg.PeerChat:
		return &tg.InputPeerChat{ChatID: v.ChatID}
	case *tg.PeerChannel:
		if h, ok := c.Peers.channel(v.ChannelID); ok {
			return &tg.InputPeerChannel{ChannelID: v.ChannelID, AccessHash: h}
		}
	}
	return &tg.InputPeerEmpty{}
}

func peerKey(p tg.PeerClass) string {
	switch v := p.(type) {
	case *tg.PeerUser:
		return "u" + strconv.FormatInt(v.UserID, 10)
	case *tg.PeerChat:
		return "c" + strconv.FormatInt(v.ChatID, 10)
	case *tg.PeerChannel:
		return "h" + strconv.FormatInt(v.ChannelID, 10)
	}
	return "?"
}

func msgKey(m tg.MessageClass) string {
	switch v := m.(type) {
	case *tg.Message:
		return peerKey(v.PeerID) + ":" + strconv.Itoa(v.ID)
	case *tg.MessageService:
		return peerKey(v.PeerID) + ":" + strconv.Itoa(v.ID)
	}
	return "?:" + strconv.Itoa(m.GetID())
}

func messageDate(m tg.MessageClass) int {
	switch v := m.(type) {
	case *tg.Message:
		return v.Date
	case *tg.MessageService:
		return v.Date
	}
	return 0
}

// MarkedPeerID — id пира в формате Bot API (-100… для каналов).
func MarkedPeerID(p tg.PeerClass) int64 {
	switch v := p.(type) {
	case *tg.PeerUser:
		return v.UserID
	case *tg.PeerChat:
		return -v.ChatID
	case *tg.PeerChannel:
		return -(channelShift + v.ChannelID)
	}
	return 0
}

// PeerOfTarget — tg.PeerClass для разрешённого чата.
func PeerOfTarget(t Target) tg.PeerClass {
	switch t.Kind {
	case "chat":
		return &tg.PeerChat{ChatID: t.ID}
	case "channel":
		return &tg.PeerChannel{ChannelID: t.ID}
	}
	return &tg.PeerUser{UserID: t.ID}
}
