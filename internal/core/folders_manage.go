package core

// Раскладка чатов по папкам и архиву — отдельное право владельца
// ([manage] folders = true в chats.toml, запрос — tg_request_access с
// manage_folders). Сообщений оно не открывает: агент видит только названия.
// Чат, который агент сам положил в папку или достал из архива, через группу
// белого списка не открывается (blockForAgent) — иначе раскладка была бы
// обходом решения владельца о допуске.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/omap"
	"tgagent/internal/tgc"
)

func canManage(s *config.Settings) error {
	if err := onlyLocal(s); err != nil {
		return err
	}
	if n := netOf(s); n != nil {
		return denied("В %s раскладывать чаты по папкам агент пока не умеет.", n.name())
	}
	if !s.Manage.Folders {
		return denied("Раскладывать чаты по папкам и архиву — отдельное право владельца. Попроси его: " +
			"tg_request_access(manage_folders=true, reason=…).")
	}
	return nil
}

// UnfiledChats — tg_unfiled_chats: чаты, которых нет ни в одной папке.
func UnfiledChats(ctx context.Context, s *config.Settings, archived string) (*omap.Map, error) {
	if err := canManage(s); err != nil {
		return nil, err
	}
	var rows []tgc.DialogRow
	known := map[int64]config.ChatRule{}
	err := watchFolders(ctx, s, func(see func([]tgc.Folder) *agentBook) error {
		return tgc.Run(ctx, s, tgc.Opts{Wait: 30 * time.Second, Caller: "unfiled_chats"}, func(ctx context.Context, c *tgc.Conn) error {
			folders, err := c.Folders(ctx)
			if err != nil {
				return err
			}
			see(folders)
			page, err := c.Dialogs(ctx)
			if err != nil {
				return err
			}
			rows = c.Unfiled(page, folders)
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
	chats := []*omap.Map{}
	for _, r := range rows {
		switch archived {
		case "only":
			if !r.Archived {
				continue
			}
		case "no":
			if r.Archived {
				continue
			}
		}
		item := omap.New().Set("id", r.ID).Set("title", r.Title).Set("kind", r.Kind)
		if r.Username != "" {
			item.Set("username", "@"+r.Username)
		}
		item.Set("unread", r.Unread).Set("archived", r.Archived)
		if rule, ok := known[r.ID]; ok {
			item.Set("alias", s.Ref(rule.Alias))
		} else {
			item.Set("alias", nil)
		}
		chats = append(chats, item)
	}
	logEvent(s, "unfiled_chats_listed", "count", len(chats), "archived", archived)
	return omap.New().Set("count", len(chats)).Set("chats", chats).Set("hint",
		"Положить в папку — tg_move_chat(chat_id, to_folder); в архив и обратно — tg_archive_chat. "+
			"alias = null — сообщений этого чата тебе не видно."), nil
}

// MoveChat — tg_move_chat: положить чат в папку to и/или вынуть из from.
func MoveChat(ctx context.Context, s *config.Settings, chatID int64, from, to string) (*omap.Map, error) {
	if err := canManage(s); err != nil {
		return nil, err
	}
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if from == "" && to == "" {
		return nil, &Bad{Msg: "Укажи to_folder (куда положить) и/или from_folder (откуда вынуть)."}
	}
	if chatID == 0 {
		return nil, &Bad{Msg: "chat_id — id чата из tg_folder_chats или tg_unfiled_chats."}
	}
	var (
		title          string
		fromF, toF     tgc.Folder
		fromOK, toOK   bool
		inFrom, inTo   bool
		alreadyMessage string
	)
	// под тем же локом, что правка и удаление папок: фильтр папки читается
	// и пишется целиком, и параллельная операция потеряла бы чужую правку
	// (или вернула бы удалённую папку)
	err := withFolders(ctx, s, func(b *agentBook) error {
		return tgc.Run(ctx, s, tgc.Opts{Wait: 30 * time.Second, Caller: "move_chat"}, func(ctx context.Context, c *tgc.Conn) error {
			info, err := c.ChatInfo(ctx, chatID)
			if err != nil {
				return &Bad{Msg: fmt.Sprintf("Чат %d не найден среди чатов аккаунта: %v", chatID, err)}
			}
			title = info.Title
			folders, err := c.Folders(ctx)
			if err != nil {
				return err
			}
			b.sync(folders)
			find := func(ref string) (tgc.Folder, bool, error) {
				if ref == "" {
					return tgc.Folder{}, false, nil
				}
				f, err := pickFolder(folders, ref)
				return f, err == nil, err
			}
			if fromF, fromOK, err = find(from); err != nil {
				return err
			}
			if toF, toOK, err = find(to); err != nil {
				return err
			}
			if fromOK && toOK && fromF.ID == toF.ID {
				return &Bad{Msg: "from_folder и to_folder — одна папка."}
			}
			page, err := c.Dialogs(ctx)
			if err != nil {
				return err
			}
			inFrom = fromOK && c.InFolder(page, fromF, chatID)
			inTo = toOK && c.InFolder(page, toF, chatID)
			if (!fromOK || !inFrom) && (!toOK || inTo) {
				alreadyMessage = "Ничего менять не нужно: чат уже так и лежит."
				return nil
			}
			if toOK && !inTo {
				if err := blockForAgent(s, chatID); err != nil {
					return err
				}
				if err := c.MoveChat(ctx, toF.ID, chatID, true); err != nil {
					return err
				}
			}
			if fromOK && inFrom {
				if err := c.MoveChat(ctx, fromF.ID, chatID, false); err != nil {
					return err
				}
			}
			return nil
		})
	})
	if err != nil {
		return nil, folderErr(err)
	}
	out := omap.New().Set("chat_id", chatID).Set("title", title)
	if fromOK {
		out.Set("from_folder", fromF.Title)
	}
	if toOK {
		out.Set("to_folder", toF.Title)
	}
	if alreadyMessage != "" {
		return out.Set("status", "unchanged").Set("hint", alreadyMessage), nil
	}
	logEvent(s, "folder_chat_moved", "chat_id", chatID, "title", title, "from", fromF.Title, "to", toF.Title)
	afterManage(ctx, s)
	out.Set("status", "moved")
	if toOK {
		if set, ok := folderSet(s, strconv.Itoa(toF.ID), toF.Title); ok && set.Read && !readableByRule(s, chatID) {
			out.Set("hint", "Папка открыта агентам, но этот чат положил ты сам — его сообщения тебе не откроются: "+
				"доступ к нему решает владелец (tg_request_access).")
		}
	}
	return out, nil
}

// ArchiveChat — tg_archive_chat.
func ArchiveChat(ctx context.Context, s *config.Settings, chatID int64, archived bool) (*omap.Map, error) {
	if err := canManage(s); err != nil {
		return nil, err
	}
	if chatID == 0 {
		return nil, &Bad{Msg: "chat_id — id чата из tg_folder_chats или tg_unfiled_chats."}
	}
	var title string
	err := tgc.Run(ctx, s, tgc.Opts{Wait: 30 * time.Second, Caller: "archive_chat"}, func(ctx context.Context, c *tgc.Conn) error {
		info, err := c.ChatInfo(ctx, chatID)
		if err != nil {
			return &Bad{Msg: fmt.Sprintf("Чат %d не найден среди чатов аккаунта: %v", chatID, err)}
		}
		title = info.Title
		// из архива чат может попасть в папку с правилом «без архива»
		if err := blockForAgent(s, chatID); err != nil {
			return err
		}
		return c.SetArchived(ctx, chatID, archived)
	})
	if err != nil {
		return nil, folderErr(err)
	}
	logEvent(s, "chat_archived", "chat_id", chatID, "title", title, "archived", archived)
	afterManage(ctx, s)
	return omap.New().Set("chat_id", chatID).Set("title", title).Set("archived", archived).Set("status", "done"), nil
}

// afterManage — состав групп белого списка пересчитать сразу.
func afterManage(ctx context.Context, s *config.Settings) {
	tgc.ForgetDialogs()
	if len(s.Sets) > 0 {
		_, _ = RefreshSets(ctx, s, true)
	}
}

func readableByRule(s *config.Settings, chatID int64) bool {
	fresh, err := config.Load()
	if err != nil {
		return false
	}
	for _, r := range fresh.Rules() {
		if rulePeerID(r) == chatID && r.Read {
			return true
		}
	}
	return false
}

func folderErr(err error) error {
	if ne, ok := err.(*tgc.NotEditable); ok {
		return &Bad{Msg: ne.Msg}
	}
	switch tgc.RPCMessage(err) {
	case "FILTER_INCLUDE_TOO_MUCH", "DIALOG_FILTERS_TOO_MUCH", "CHATLIST_INCLUDE_INVALID":
		return &Bad{Msg: "Telegram не принял: в папке уже предел явно добавленных чатов (100, с Premium — 200)."}
	case "FILTER_INCLUDE_EMPTY":
		return &Bad{Msg: "Telegram не оставляет папку пустой: в ней не останется ни чатов, ни правил. " +
			"Удалить папку — tg_delete_folder."}
	}
	return err
}
