package tgc

// Группы чатов для белого списка и раскладка чатов по папкам и архиву.

import (
	"context"
	"fmt"
	"slices"

	"github.com/gotd/td/tg"
)

// Dialogs — все диалоги аккаунта (из минутного кэша).
func (c *Conn) Dialogs(ctx context.Context) (*DialogPage, error) { return c.dialogsFresh(ctx, 0) }

// ForgetDialogs — сбросить кэш диалогов: чат переложили в архив и т.п.
func ForgetDialogs() {
	dialogsCache.Lock()
	dialogsCache.page = nil
	dialogsCache.Unlock()
}

// PublicChannels — каналы аккаунта с публичным @username: не по
// ссылке-приглашению, не группы и не лички.
func (c *Conn) PublicChannels(page *DialogPage) []DialogRow {
	var out []DialogRow
	for _, d := range page.Dialogs {
		p, ok := d.Peer.(*tg.PeerChannel)
		if !ok {
			continue
		}
		ch, ok := page.Chats[p.ChannelID].(*tg.Channel)
		if !ok || !ch.Broadcast || ch.Left {
			continue
		}
		if row := dialogRow(d, page); row.Username != "" {
			out = append(out, row)
		}
	}
	return out
}

// Unfiled — чаты, которых нет ни в одной папке (в архиве — с Archived).
func (c *Conn) Unfiled(page *DialogPage, folders []Folder) []DialogRow {
	self := c.Peers.SelfID()
	var out []DialogRow
	for _, d := range page.Dialogs {
		f := facts(d, page, self)
		if !slices.ContainsFunc(folders, func(folder Folder) bool { return folder.contains(f) }) {
			out = append(out, dialogRow(d, page))
		}
	}
	return out
}

// InFolder — входит ли чат (id формата Bot API) в папку.
func (c *Conn) InFolder(page *DialogPage, f Folder, marked int64) bool {
	self := c.Peers.SelfID()
	for _, d := range page.Dialogs {
		if MarkedPeerID(d.Peer) == marked {
			return f.contains(facts(d, page, self))
		}
	}
	return false
}

// NotEditable — так папку не поменять (папка-ссылка, нет такой).
type NotEditable struct{ Msg string }

func (e *NotEditable) Error() string { return e.Msg }

// MoveChat — положить чат в папку (add) или вынуть из неё. Как в клиентах
// Telegram: вынуть чат, который попадает в папку по её правилам («все
// каналы»), — значит добавить его в исключения.
func (c *Conn) MoveChat(ctx context.Context, folderID int, marked int64, add bool) error {
	res, err := c.API.MessagesGetDialogFilters(ctx)
	if err != nil {
		return err
	}
	var filter *tg.DialogFilter
	for _, f := range res.Filters {
		switch v := f.(type) {
		case *tg.DialogFilter:
			if v.ID == folderID {
				filter = v
			}
		case *tg.DialogFilterChatlist:
			if v.ID == folderID {
				return &NotEditable{"Это папка-ссылка: её состав раздаётся по приглашению, менять его может только владелец."}
			}
		}
	}
	if filter == nil {
		return &NotEditable{fmt.Sprintf("Папки %d нет — список: tg_list_folders.", folderID)}
	}
	t, err := c.resolveID(ctx, marked)
	if err != nil {
		return err
	}
	input := t.Input
	if t.Kind == "self" {
		input = &tg.InputPeerSelf{}
	}
	self := c.Peers.SelfID()
	same := func(p tg.InputPeerClass) bool { return inputMarkedID(p, self) == marked }
	drop := func(list []tg.InputPeerClass) []tg.InputPeerClass {
		return slices.DeleteFunc(slices.Clone(list), same)
	}
	page, err := c.dialogsFresh(ctx, 0)
	if err != nil {
		return err
	}
	rules := Folder{Contacts: filter.Contacts, NonContacts: filter.NonContacts, Groups: filter.Groups,
		Broadcasts: filter.Broadcasts, Bots: filter.Bots, ExcludeMuted: filter.ExcludeMuted,
		ExcludeRead: filter.ExcludeRead, ExcludeArchived: filter.ExcludeArchived}
	byRules := c.InFolder(page, rules, marked)
	filter.ExcludePeers = drop(filter.ExcludePeers)
	if add {
		if !byRules && !slices.ContainsFunc(filter.IncludePeers, same) && !slices.ContainsFunc(filter.PinnedPeers, same) {
			filter.IncludePeers = append(filter.IncludePeers, input)
		}
	} else {
		filter.IncludePeers = drop(filter.IncludePeers)
		filter.PinnedPeers = drop(filter.PinnedPeers)
		if byRules {
			filter.ExcludePeers = append(filter.ExcludePeers, input)
		}
	}
	req := &tg.MessagesUpdateDialogFilterRequest{ID: folderID}
	req.SetFilter(filter)
	_, err = c.API.MessagesUpdateDialogFilter(ctx, req)
	return err
}

// SetArchived — убрать чат в архив или достать из него.
func (c *Conn) SetArchived(ctx context.Context, marked int64, archived bool) error {
	t, err := c.resolveID(ctx, marked)
	if err != nil {
		return err
	}
	input := t.Input
	if t.Kind == "self" {
		input = &tg.InputPeerSelf{}
	}
	folder := 0
	if archived {
		folder = 1
	}
	_, err = c.API.FoldersEditPeerFolders(ctx, []tg.InputFolderPeer{{Peer: input, FolderID: folder}})
	ForgetDialogs()
	return err
}

// MarkChannelRead — отметить посты канала прочитанными до maxID включительно.
func (c *Conn) MarkChannelRead(ctx context.Context, t Target, maxID int) error {
	in, ok := t.Input.(*tg.InputPeerChannel)
	if !ok {
		return fmt.Errorf("это не канал")
	}
	_, err := c.API.ChannelsReadHistory(ctx, &tg.ChannelsReadHistoryRequest{
		Channel: &tg.InputChannel{ChannelID: in.ChannelID, AccessHash: in.AccessHash}, MaxID: maxID})
	return err
}
