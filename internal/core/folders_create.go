package core

// Создание, правка и удаление папок Telegram — то же право, что раскладка
// ([manage] folders). Граница та же: папкой агент не открывает себе чатов.
//
//   - Папка, на которую у владельца есть правило [[folder]] (по id или
//     названию), — его решение о доступе: её название и правила агент не
//     меняет и её не удаляет. Скрыть в ней чат можно (это закрывает), вернуть
//     скрытый — только с blockForAgent, как при раскладке.
//   - Новая папка и новое название не совпадают ни с одним правилом владельца:
//     ни номером (правило по id на удалённую папку), ни названием — иначе
//     агент сам собрал бы открытую агентам папку.
//   - Удалить можно только созданную агентом папку или папку без чатов:
//     чужая раскладка владельца — его работа. Созданные агентом — в
//     data/agent_folders.json (номер и название). Учёт сверяется со списком
//     папок при каждом обращении: папки нет или у неё другое название —
//     запись снимается насовсем (владелец переименовал её или удалил, и
//     вернуть название обратно не значит вернуть папку агенту).
//   - Операции с папками — по одной (лок на agent_folders.json): номер новой
//     папки берётся по списку, и две разом взяли бы один — вторая
//     перезаписала бы первую.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"tgagent/internal/config"
	"tgagent/internal/fsx"
	"tgagent/internal/lock"
	"tgagent/internal/omap"
	"tgagent/internal/tgc"
)

// FolderOpts — папка для tg_create_folder / tg_edit_folder.
type FolderOpts struct {
	Title string
	Emoji *string
	// Include, Exclude — правила словами; nil — не менять (при создании — нет)
	Include, Exclude []string
	Chats            []int64 // при создании — явно добавленные чаты
	Hide, Unhide     []int64 // при правке — в «Не показывать» и обратно
}

// folderTitleMax — предел названия папки в Telegram, символов.
const folderTitleMax = 12

type agentFolder struct {
	ID      int       `json:"id"`
	Title   string    `json:"title"`
	Created time.Time `json:"created"`
}

func agentFoldersPath() string { return filepath.Join(config.DataPath(), "agent_folders.json") }

func loadAgentFolders() []agentFolder {
	var out []agentFolder
	if raw, err := os.ReadFile(agentFoldersPath()); err == nil {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// agentBook — учёт папок агента на время одной операции (под локом).
type agentBook struct {
	list  []agentFolder
	dirty bool
}

// withFolders — операция с папками под локом; учёт пишется в конце, даже
// если операция не удалась (сверка со списком уже могла снять записи).
func withFolders(ctx context.Context, s *config.Settings, fn func(*agentBook) error) error {
	path := agentFoldersPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return lock.With(ctx, path, 2*time.Minute, func() error {
		b := &agentBook{list: loadAgentFolders()}
		err := fn(b)
		if b.dirty {
			if serr := saveAgentFolders(b.list); serr != nil {
				logEvent(s, "agent_folders_save_failed", "error", serr.Error())
			}
		}
		return err
	})
}

func saveAgentFolders(list []agentFolder) error {
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFile(agentFoldersPath(), raw, 0o600)
}

// sync — снять записи, которым в списке папок нет пары номер + название.
func (b *agentBook) sync(folders []tgc.Folder) {
	n := len(b.list)
	b.list = slices.DeleteFunc(b.list, func(a agentFolder) bool {
		return !slices.ContainsFunc(folders, func(f tgc.Folder) bool { return f.ID == a.ID && f.Title == a.Title })
	})
	b.dirty = b.dirty || len(b.list) != n
}

func (b *agentBook) mine(f tgc.Folder) bool {
	return slices.ContainsFunc(b.list, func(a agentFolder) bool { return a.ID == f.ID && a.Title == f.Title })
}

func (b *agentBook) put(id int, title string) {
	b.drop(id)
	b.list = append(b.list, agentFolder{ID: id, Title: title, Created: time.Now()})
	b.dirty = true
}

func (b *agentBook) rename(id int, title string) {
	for i := range b.list {
		if b.list[i].ID == id {
			b.list[i].Title, b.dirty = title, true
		}
	}
}

func (b *agentBook) drop(id int) {
	n := len(b.list)
	b.list = slices.DeleteFunc(b.list, func(a agentFolder) bool { return a.ID == id })
	b.dirty = b.dirty || len(b.list) != n
}

// listedFolders — список папок (fetch) и какие из них завёл агент (для
// tg_list_folders). Есть учёт — список берётся уже под локом: сверка по
// снимку до лока сняла бы запись, которую записала параллельная операция.
func listedFolders(ctx context.Context, s *config.Settings, fetch func() ([]tgc.Folder, error)) ([]tgc.Folder, map[int]bool, error) {
	out := map[int]bool{}
	var folders []tgc.Folder
	err := watchFolders(ctx, s, func(see func([]tgc.Folder) *agentBook) error {
		var err error
		if folders, err = fetch(); err != nil {
			return err
		}
		if b := see(folders); b != nil {
			for _, f := range folders {
				if b.mine(f) {
					out[f.ID] = true
				}
			}
		}
		return nil
	})
	return folders, out, err
}

// watchFolders — операция агента, которая видит список папок (чтение
// состава, чаты вне папок): каждый увиденный список сверяется с учётом
// (see), иначе увиденное агентом переименование не сняло бы запись.
// Учёт пуст — без лока, see ничего не делает и отдаёт nil. Фоновый
// пересчёт групп (RunSets) сверку не делает: он держит setsMu, а правка
// папок берёт setsMu (blockForAgent) уже под этим локом.
func watchFolders(ctx context.Context, s *config.Settings, fn func(see func([]tgc.Folder) *agentBook) error) error {
	if len(loadAgentFolders()) == 0 {
		return fn(func([]tgc.Folder) *agentBook { return nil })
	}
	return withFolders(ctx, s, func(b *agentBook) error {
		return fn(func(folders []tgc.Folder) *agentBook { b.sync(folders); return b })
	})
}

var (
	folderInclude = map[string]func(*tgc.FolderRules){
		"contacts":     func(r *tgc.FolderRules) { r.Contacts = true },
		"non_contacts": func(r *tgc.FolderRules) { r.NonContacts = true },
		"groups":       func(r *tgc.FolderRules) { r.Groups = true },
		"channels":     func(r *tgc.FolderRules) { r.Broadcasts = true },
		"bots":         func(r *tgc.FolderRules) { r.Bots = true },
	}
	folderExclude = map[string]func(*tgc.FolderRules){
		"muted":    func(r *tgc.FolderRules) { r.ExcludeMuted = true },
		"read":     func(r *tgc.FolderRules) { r.ExcludeRead = true },
		"archived": func(r *tgc.FolderRules) { r.ExcludeArchived = true },
	}
)

func parseFolderRules(include, exclude []string) (tgc.FolderRules, bool, error) {
	var r tgc.FolderRules
	any := false
	for _, part := range []struct {
		words []string
		known map[string]func(*tgc.FolderRules)
		field string
	}{{include, folderInclude, "include"}, {exclude, folderExclude, "exclude"}} {
		for _, w := range part.words {
			set, ok := part.known[strings.ToLower(strings.TrimSpace(w))]
			if !ok {
				var names []string
				for k := range part.known {
					names = append(names, k)
				}
				slices.Sort(names)
				return r, false, &Bad{Msg: fmt.Sprintf("%s: «%s» не знаю; можно %s.", part.field, w, strings.Join(names, ", "))}
			}
			set(&r)
			if part.field == "include" {
				any = true
			}
		}
	}
	return r, any, nil
}

// pickFolder — папка по id или названию.
func pickFolder(folders []tgc.Folder, ref string) (tgc.Folder, error) {
	ref = strings.TrimSpace(ref)
	f, ok := findTGFolder(folders, ref, "")
	if !ok {
		f, ok = findTGFolder(folders, "", ref)
	}
	if ok {
		return f, nil
	}
	var names []string
	for _, f := range folders {
		names = append(names, fmt.Sprintf("%d «%s»", f.ID, f.Title))
	}
	return f, &Bad{Msg: fmt.Sprintf("Папки «%s» нет. Есть: %s.", ref, strings.Join(names, ", "))}
}

// titleFree — название не занято другой папкой и не названо в правиле
// владельца.
func titleFree(s *config.Settings, folders []tgc.Folder, title string, self int) error {
	// длиннее Telegram отвечает MESSAGE_TOO_LONG (проверено 12.11.2026)
	if n := utf8.RuneCountInString(title); n > folderTitleMax {
		return &Bad{Msg: fmt.Sprintf("Название папки — не длиннее %d символов, а в «%s» их %d.", folderTitleMax, title, n)}
	}
	for _, set := range s.Sets {
		if !set.Public && set.Folder == "" && strings.EqualFold(strings.TrimSpace(set.Name), title) {
			return denied("На папку «%s» у владельца есть решение о доступе агентам — такую папку агент не заводит: "+
				"возьми другое название.", title)
		}
	}
	for _, f := range folders {
		if f.ID != self && strings.EqualFold(f.Title, title) {
			return &Bad{Msg: fmt.Sprintf("Папка «%s» уже есть (id %d).", f.Title, f.ID)}
		}
	}
	return nil
}

// freeFolderID — номер для новой папки: не занят и не назван в правиле
// владельца (правило по id на удалённую папку открыло бы новую).
func freeFolderID(s *config.Settings, folders []tgc.Folder) (int, error) {
	for id := 2; id <= 255; id++ {
		if slices.ContainsFunc(folders, func(f tgc.Folder) bool { return f.ID == id }) {
			continue
		}
		if slices.ContainsFunc(s.Sets, func(set config.ChatSet) bool { return !set.Public && set.Folder == strconv.Itoa(id) }) {
			continue
		}
		return id, nil
	}
	return 0, &Bad{Msg: "Свободного номера папки нет."}
}

func ownerFolder(s *config.Settings, f tgc.Folder) bool {
	_, ok := folderSet(s, strconv.Itoa(f.ID), f.Title)
	return ok
}

// CreateFolder — tg_create_folder.
func CreateFolder(ctx context.Context, s *config.Settings, o FolderOpts) (*omap.Map, error) {
	if err := canManage(s); err != nil {
		return nil, err
	}
	title := strings.TrimSpace(o.Title)
	if title == "" {
		return nil, &Bad{Msg: "Укажи title — название папки."}
	}
	rules, anyRule, err := parseFolderRules(o.Include, o.Exclude)
	if err != nil {
		return nil, err
	}
	if !anyRule && len(o.Chats) == 0 {
		return nil, &Bad{Msg: "Пустую папку Telegram не заводит: дай chats (id из tg_unfiled_chats или tg_folder_chats) и/или include."}
	}
	emoji := ""
	if o.Emoji != nil {
		emoji = strings.TrimSpace(*o.Emoji)
	}
	var id int
	err = withFolders(ctx, s, func(b *agentBook) error {
		return tgc.Run(ctx, s, tgc.Opts{Wait: 30 * time.Second, Caller: "create_folder"}, func(ctx context.Context, c *tgc.Conn) error {
			folders, err := c.Folders(ctx)
			if err != nil {
				return err
			}
			b.sync(folders)
			if err := titleFree(s, folders, title, -1); err != nil {
				return err
			}
			if id, err = freeFolderID(s, folders); err != nil {
				return err
			}
			if err := c.CreateFolder(ctx, id, title, emoji, rules, o.Chats); err != nil {
				return err
			}
			b.put(id, title)
			return nil
		})
	})
	if err != nil {
		if tgc.RPCMessage(err) == "DIALOG_FILTERS_TOO_MUCH" {
			return nil, &Bad{Msg: "Telegram не принял: у аккаунта уже предел папок (10, с Premium — 30)."}
		}
		return nil, folderErr(err)
	}
	logEvent(s, "folder_created", "folder", id, "title", title, "chats", len(o.Chats))
	afterManage(ctx, s)
	return omap.New().Set("id", id).Set("title", title).Set("status", "created").Set("hint",
		"Чаты — tg_folder_chats(folder), добавить — tg_move_chat(to_folder), скрыть — tg_edit_folder(hide_chats). "+
			"Доступа к сообщениям её чатов папка не даёт."), nil
}

// EditFolder — tg_edit_folder.
func EditFolder(ctx context.Context, s *config.Settings, ref string, o FolderOpts) (*omap.Map, error) {
	if err := canManage(s); err != nil {
		return nil, err
	}
	if strings.TrimSpace(ref) == "" {
		return nil, &Bad{Msg: "Укажи folder — id или название из tg_list_folders."}
	}
	title := strings.TrimSpace(o.Title)
	edit := tgc.FolderEdit{Title: title, Hide: o.Hide, Unhide: o.Unhide}
	if o.Emoji != nil {
		e := strings.TrimSpace(*o.Emoji)
		edit.Emoticon = &e
	}
	if o.Include != nil || o.Exclude != nil {
		rules, _, err := parseFolderRules(o.Include, o.Exclude)
		if err != nil {
			return nil, err
		}
		// не указанная часть правил остаётся как была
		if o.Include != nil {
			edit.Include = &rules
		}
		if o.Exclude != nil {
			edit.Exclude = &rules
		}
	}
	for _, id := range o.Hide {
		if slices.Contains(o.Unhide, id) {
			return nil, &Bad{Msg: fmt.Sprintf("Чат %d и в hide_chats, и в unhide_chats.", id)}
		}
	}
	reshape := title != "" || edit.Emoticon != nil || edit.Include != nil || edit.Exclude != nil
	if !reshape && len(o.Hide) == 0 && len(o.Unhide) == 0 {
		return nil, &Bad{Msg: "Нечего менять: title, emoji, include/exclude, hide_chats или unhide_chats."}
	}
	var f tgc.Folder
	err := withFolders(ctx, s, func(b *agentBook) error {
		return tgc.Run(ctx, s, tgc.Opts{Wait: 30 * time.Second, Caller: "edit_folder"}, func(ctx context.Context, c *tgc.Conn) error {
			folders, err := c.Folders(ctx)
			if err != nil {
				return err
			}
			b.sync(folders)
			if f, err = pickFolder(folders, ref); err != nil {
				return err
			}
			owner := ownerFolder(s, f)
			if reshape && owner {
				return denied("Папка «%s» открыта агентам решением владельца: её название, значок и правила меняет он. "+
					"Скрыть в ней чат можно (hide_chats).", f.Title)
			}
			if title != "" {
				if err := titleFree(s, folders, title, f.ID); err != nil {
					return err
				}
			}
			if owner {
				// вернуть скрытый чат в открытую папку — как положить его туда
				for _, id := range o.Unhide {
					if err := blockForAgent(s, id); err != nil {
						return err
					}
				}
			}
			if err := c.EditFolder(ctx, f.ID, edit); err != nil {
				return err
			}
			if title != "" && b.mine(f) {
				b.rename(f.ID, title)
			}
			return nil
		})
	})
	if err != nil {
		return nil, folderErr(err)
	}
	logEvent(s, "folder_edited", "folder", f.ID, "title", f.Title, "new_title", title,
		"rules", edit.Include != nil || edit.Exclude != nil, "hidden", len(o.Hide), "unhidden", len(o.Unhide))
	afterManage(ctx, s)
	out := omap.New().Set("id", f.ID).Set("title", f.Title).Set("status", "edited")
	if title != "" {
		out.Set("title", title)
	}
	if len(o.Unhide) > 0 && ownerFolder(s, f) {
		out.Set("hint", "Папка открыта агентам, но возвращённые тобой чаты доступа к сообщениям не дают: о допуске решает владелец.")
	}
	return out, nil
}

// DeleteFolder — tg_delete_folder.
func DeleteFolder(ctx context.Context, s *config.Settings, ref string) (*omap.Map, error) {
	if err := canManage(s); err != nil {
		return nil, err
	}
	if strings.TrimSpace(ref) == "" {
		return nil, &Bad{Msg: "Укажи folder — id или название из tg_list_folders."}
	}
	var f tgc.Folder
	err := withFolders(ctx, s, func(b *agentBook) error {
		return tgc.Run(ctx, s, tgc.Opts{Wait: 30 * time.Second, Caller: "delete_folder"}, func(ctx context.Context, c *tgc.Conn) error {
			folders, err := c.Folders(ctx)
			if err != nil {
				return err
			}
			b.sync(folders)
			if f, err = pickFolder(folders, ref); err != nil {
				return err
			}
			if f.Shared {
				return denied("«%s» — папка-ссылка: удалить её может только владелец.", f.Title)
			}
			if ownerFolder(s, f) {
				return denied("Папка «%s» открыта агентам решением владельца: удалить её может только он.", f.Title)
			}
			if !b.mine(f) {
				// папка владельца: только если в ней не осталось ни правил
				// (пустая «Непрочитанные» пуста лишь до нового сообщения), ни
				// явно добавленных чатов среди свежего (не кэшированного) списка
				if f.Contacts || f.NonContacts || f.Groups || f.Broadcasts || f.Bots {
					return denied("Папку «%s» завёл владелец, и чаты попадают в неё по правилам: удалить её может только он.", f.Title)
				}
				tgc.ForgetDialogs()
				page, err := c.Dialogs(ctx)
				if err != nil {
					return err
				}
				explicit := append(slices.Clone(f.Include), f.Pinned...)
				n := 0
				for _, d := range page.Dialogs {
					if slices.Contains(explicit, tgc.MarkedPeerID(d.Peer)) {
						n++
					}
				}
				if n > 0 {
					return denied("Папку «%s» завёл владелец, и чатов в ней — %d: удалить её может только он.", f.Title, n)
				}
			}
			if err := c.DeleteFolder(ctx, f.ID); err != nil {
				return err
			}
			b.drop(f.ID)
			return nil
		})
	})
	if err != nil {
		return nil, folderErr(err)
	}
	logEvent(s, "folder_deleted", "folder", f.ID, "title", f.Title)
	afterManage(ctx, s)
	return omap.New().Set("id", f.ID).Set("title", f.Title).Set("status", "deleted").Set("hint",
		"Чаты папки остались в списке чатов."), nil
}
