package tgc

// Группы чатов для белого списка и раскладка чатов по папкам и архиву.

import (
	"context"
	"fmt"
	"slices"
	"strings"

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

// filterOf — папка как её хранит Telegram; папку-ссылку и чужую — нельзя.
func (c *Conn) filterOf(ctx context.Context, folderID int) (*tg.DialogFilter, error) {
	f, err := c.anyFilter(ctx, folderID)
	if err != nil {
		return nil, err
	}
	if v, ok := f.(*tg.DialogFilter); ok {
		return v, nil
	}
	return nil, &NotEditable{"Это папка-ссылка: её название и правила так не меняются. Состав своей " +
		"папки-ссылки — tg_move_chat; чужую можно только покинуть — tg_delete_folder (кнопкой владельца)."}
}

// anyFilter — папка с номером folderID: обычная или папка-ссылка.
func (c *Conn) anyFilter(ctx context.Context, folderID int) (tg.DialogFilterClass, error) {
	res, err := c.API.MessagesGetDialogFilters(ctx)
	if err != nil {
		return nil, err
	}
	for _, f := range res.Filters {
		switch v := f.(type) {
		case *tg.DialogFilter:
			if v.ID == folderID {
				return v, nil
			}
		case *tg.DialogFilterChatlist:
			if v.ID == folderID {
				return v, nil
			}
		}
	}
	return nil, &NotEditable{fmt.Sprintf("Папки %d нет — список: tg_list_folders.", folderID)}
}

// LeaveChatlist — покинуть папку-ссылку, полученную по чужой ссылке. Чаты
// папки остаются: выходить из них Telegram здесь не заставляет (peers пуст).
func (c *Conn) LeaveChatlist(ctx context.Context, folderID int) error {
	_, err := c.API.ChatlistsLeaveChatlist(ctx, &tg.ChatlistsLeaveChatlistRequest{
		Chatlist: tg.InputChatlistDialogFilter{FilterID: folderID}, Peers: []tg.InputPeerClass{}})
	ForgetDialogs()
	return err
}

// inputOf — чат (id формата Bot API) для списков папки.
func (c *Conn) inputOf(ctx context.Context, marked int64) (tg.InputPeerClass, error) {
	t, err := c.resolveID(ctx, marked)
	if err != nil {
		return nil, err
	}
	if t.Kind == "self" {
		return &tg.InputPeerSelf{}, nil
	}
	return t.Input, nil
}

func (c *Conn) putFilter(ctx context.Context, filter *tg.DialogFilter) error {
	req := &tg.MessagesUpdateDialogFilterRequest{ID: filter.ID}
	req.SetFilter(filter)
	_, err := c.API.MessagesUpdateDialogFilter(ctx, req)
	ForgetDialogs()
	return err
}

// MoveChat — положить чат в папку (add) или вынуть из неё. Как в клиентах
// Telegram: вынуть чат, который попадает в папку по её правилам («все
// каналы»), — значит добавить его в исключения.
func (c *Conn) MoveChat(ctx context.Context, folderID int, marked int64, add bool) error {
	got, err := c.anyFilter(ctx, folderID)
	if err != nil {
		return err
	}
	input, err := c.inputOf(ctx, marked)
	if err != nil {
		return err
	}
	self := c.Peers.SelfID()
	same := func(p tg.InputPeerClass) bool { return inputMarkedID(p, self) == marked }
	drop := func(list []tg.InputPeerClass) []tg.InputPeerClass {
		return slices.DeleteFunc(slices.Clone(list), same)
	}
	if list, ok := got.(*tg.DialogFilterChatlist); ok {
		return c.moveInChatlist(ctx, list, input, same, drop, add)
	}
	filter := got.(*tg.DialogFilter)
	page, err := c.dialogsFresh(ctx, 0)
	if err != nil {
		return err
	}
	byRules := c.InFolder(page, rulesOf(filter), marked)
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
	return c.putFilter(ctx, filter)
}

// moveInChatlist — состав папки-ссылки: правил и исключений у неё нет, только
// явные чаты. Менять можно только свою (её раздаёт владелец, HasMyInvites):
// у чужой состав задаёт автор. Ссылки-приглашения при этом не меняются —
// раздать новые чаты другим владелец решает сам в Telegram.
func (c *Conn) moveInChatlist(ctx context.Context, list *tg.DialogFilterChatlist, input tg.InputPeerClass,
	same func(tg.InputPeerClass) bool, drop func([]tg.InputPeerClass) []tg.InputPeerClass, add bool) error {
	if !list.HasMyInvites {
		return &NotEditable{fmt.Sprintf("«%s» — папка-ссылка по чужому приглашению: её состав задаёт автор. "+
			"Покинуть её — tg_delete_folder (кнопкой владельца).", strings.TrimSpace(list.Title.Text))}
	}
	if add {
		if !slices.ContainsFunc(list.IncludePeers, same) && !slices.ContainsFunc(list.PinnedPeers, same) {
			list.IncludePeers = append(list.IncludePeers, input)
		}
	} else {
		list.IncludePeers = drop(list.IncludePeers)
		list.PinnedPeers = drop(list.PinnedPeers)
	}
	req := &tg.MessagesUpdateDialogFilterRequest{ID: list.ID}
	req.SetFilter(list)
	_, err := c.API.MessagesUpdateDialogFilter(ctx, req)
	ForgetDialogs()
	return err
}

func rulesOf(f *tg.DialogFilter) Folder {
	return Folder{Contacts: f.Contacts, NonContacts: f.NonContacts, Groups: f.Groups,
		Broadcasts: f.Broadcasts, Bots: f.Bots, ExcludeMuted: f.ExcludeMuted,
		ExcludeRead: f.ExcludeRead, ExcludeArchived: f.ExcludeArchived}
}

// FolderRules — правила папки: какие чаты в неё попадают сами.
type FolderRules struct {
	Contacts, NonContacts, Groups, Broadcasts, Bots bool
	ExcludeMuted, ExcludeRead, ExcludeArchived      bool
}

// apply — только через сеттеры: SetFlags при кодировании биты ставит, но не
// снимает, и снятое правило полученной папки осталось бы включённым.
func (r FolderRules) apply(f *tg.DialogFilter) {
	r.applyInclude(f)
	r.applyExclude(f)
}

func (r FolderRules) applyInclude(f *tg.DialogFilter) {
	f.SetContacts(r.Contacts)
	f.SetNonContacts(r.NonContacts)
	f.SetGroups(r.Groups)
	f.SetBroadcasts(r.Broadcasts)
	f.SetBots(r.Bots)
}

func (r FolderRules) applyExclude(f *tg.DialogFilter) {
	f.SetExcludeMuted(r.ExcludeMuted)
	f.SetExcludeRead(r.ExcludeRead)
	f.SetExcludeArchived(r.ExcludeArchived)
}

// setEmoticon — значок. Пустой — тоже с битом флага: без бита Telegram
// оставляет прежний значок, а пустая строка его снимает (проверено
// 12.11.2026).
func setEmoticon(f *tg.DialogFilter, e string) {
	f.SetEmoticon(e)
}

// FolderEdit — что поменять в папке; nil и пустое — не трогать.
type FolderEdit struct {
	Title    string
	Emoticon *string
	// Include — заменить правила включения (Contacts…Bots), Exclude —
	// исключения правил (ExcludeMuted…): одно без другого не трогает другое
	Include, Exclude *FolderRules
	// Hide — в «Не показывать» (и вон из явно добавленных), Unhide — оттуда
	Hide, Unhide []int64
}

// CreateFolder — новая папка с номером id: правила и явно добавленные чаты.
func (c *Conn) CreateFolder(ctx context.Context, id int, title, emoticon string, rules FolderRules, chats []int64) error {
	filter := &tg.DialogFilter{ID: id, Title: tg.TextWithEntities{Text: title}}
	setEmoticon(filter, emoticon)
	rules.apply(filter)
	for _, marked := range chats {
		input, err := c.inputOf(ctx, marked)
		if err != nil {
			return fmt.Errorf("чат %d: %w", marked, err)
		}
		filter.IncludePeers = append(filter.IncludePeers, input)
	}
	return c.putFilter(ctx, filter)
}

// EditFolder — переименовать, сменить правила, скрыть чаты или вернуть их.
func (c *Conn) EditFolder(ctx context.Context, folderID int, e FolderEdit) error {
	filter, err := c.filterOf(ctx, folderID)
	if err != nil {
		return err
	}
	if e.Title != "" {
		filter.Title = tg.TextWithEntities{Text: e.Title}
	}
	if e.Emoticon != nil {
		setEmoticon(filter, *e.Emoticon)
	}
	if e.Include != nil {
		e.Include.applyInclude(filter)
	}
	if e.Exclude != nil {
		e.Exclude.applyExclude(filter)
	}
	self := c.Peers.SelfID()
	in := func(list []int64) func(tg.InputPeerClass) bool {
		return func(p tg.InputPeerClass) bool { return slices.Contains(list, inputMarkedID(p, self)) }
	}
	if len(e.Unhide) > 0 {
		filter.ExcludePeers = slices.DeleteFunc(filter.ExcludePeers, in(e.Unhide))
	}
	if len(e.Hide) > 0 {
		filter.IncludePeers = slices.DeleteFunc(filter.IncludePeers, in(e.Hide))
		filter.PinnedPeers = slices.DeleteFunc(filter.PinnedPeers, in(e.Hide))
		for _, marked := range e.Hide {
			if slices.ContainsFunc(filter.ExcludePeers, in([]int64{marked})) {
				continue
			}
			input, err := c.inputOf(ctx, marked)
			if err != nil {
				return fmt.Errorf("чат %d: %w", marked, err)
			}
			filter.ExcludePeers = append(filter.ExcludePeers, input)
		}
	}
	return c.putFilter(ctx, filter)
}

// DeleteFolder — удалить папку (чаты остаются в списке чатов).
func (c *Conn) DeleteFolder(ctx context.Context, folderID int) error {
	if _, err := c.filterOf(ctx, folderID); err != nil {
		return err
	}
	_, err := c.API.MessagesUpdateDialogFilter(ctx, &tg.MessagesUpdateDialogFilterRequest{ID: folderID})
	ForgetDialogs()
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
