package config

// Доступ на группу чатов одним правилом: папка Telegram или все публичные
// каналы, на которые подписан аккаунт. Состав группы меняется вместе с
// аккаунтом (новый канал, чат положили в папку) — владелец решает один раз
// о группе, а не о каждом чате.
//
// В chats.toml:
//
//	[[folder]]            # папка: по id (надёжнее) или по названию
//	id = 3
//	name = "Новости"
//	read = true
//	send = false
//	exclude = [-1001234567890, "@spam"]
//
//	[public_channels]     # каналы с @username (не по ссылке-приглашению), только чтение
//	read = true
//
//	[manage]
//	folders = true        # агент может раскладывать чаты по папкам и архиву
//
// Кто сейчас в группе, пишет служба (data/chat_sets.json, core.RefreshSets);
// права берутся из chats.toml при каждом чтении настроек, поэтому снятое
// правило закрывает чаты сразу, а не после пересчёта состава.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"tgagent/internal/fsx"

	"github.com/BurntSushi/toml"
)

const PublicChannelsKey = "public_channels"

// ChatSet — правило белого списка на группу чатов.
type ChatSet struct {
	Key     string // "folder:<id>", "folder:name:<название>" или PublicChannelsKey
	Folder  string // id папки ("" — задана названием)
	Name    string // название папки
	Public  bool   // все публичные каналы
	Read    bool
	Send    bool
	Auto    bool
	Exclude []string // id чатов или @username
	Note    string
}

// AutoSend — как у ChatRule.
func (c ChatSet) AutoSend() bool { return c.Send && c.Auto }

// Label — как группа называется для людей и агентов.
func (c ChatSet) Label() string {
	if c.Public {
		return "публичные каналы"
	}
	if c.Name != "" {
		return "папка «" + c.Name + "»"
	}
	return "папка " + c.Folder
}

// Excluded — чат исключён из группы владельцем.
func (c ChatSet) Excluded(peer int64, username string) bool {
	for _, e := range c.Exclude {
		e = strings.TrimSpace(e)
		if n, err := strconv.ParseInt(e, 10, 64); err == nil {
			if n == peer {
				return true
			}
			continue
		}
		if username != "" && strings.EqualFold(strings.TrimPrefix(e, "@"), username) {
			return true
		}
	}
	return false
}

// FolderKey — ключ группы-папки.
func FolderKey(id, name string) string {
	if id = strings.TrimSpace(id); id != "" {
		return "folder:" + id
	}
	return "folder:name:" + strings.ToLower(strings.TrimSpace(name))
}

// Manage — права агента вне белого списка чатов.
type Manage struct {
	Folders bool // раскладывать чаты по папкам и архиву, видеть чаты вне папок
}

// LoadSets читает из chats.toml правила на группы и [manage].
func LoadSets(path string) ([]ChatSet, Manage, error) {
	var m Manage
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, m, nil
	}
	if err != nil {
		return nil, m, err
	}
	var raw struct {
		Folder []map[string]any `toml:"folder"`
		Public map[string]any   `toml:"public_channels"`
		Manage map[string]any   `toml:"manage"`
	}
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return nil, m, cfgErr("%s: %v", path, err)
	}
	var sets []ChatSet
	seen := map[string]bool{}
	for _, entry := range raw.Folder {
		set := ChatSet{
			Name: strings.TrimSpace(fmt.Sprint(valueOr(entry, "name", ""))),
			Read: truthy(valueOr(entry, "read", true)),
			Send: truthy(valueOr(entry, "send", false)),
			Auto: truthy(valueOr(entry, "auto_send", false)),
			Note: fmt.Sprint(valueOr(entry, "note", "")),
		}
		switch v := valueOr(entry, "id", "").(type) {
		case int64:
			set.Folder = strconv.FormatInt(v, 10)
		case string:
			set.Folder = strings.TrimSpace(v) // у папок MAX id — строка
		default:
			return nil, m, cfgErr("%s: id папки — число или строка из tg_list_folders", path)
		}
		if set.Folder == "" && set.Name == "" {
			return nil, m, cfgErr("%s: у [[folder]] нужен id или name", path)
		}
		set.Key = FolderKey(set.Folder, set.Name)
		if seen[set.Key] {
			return nil, m, cfgErr("%s: %s описана дважды", path, set.Label())
		}
		seen[set.Key] = true
		ex, err := excludeList(entry["exclude"])
		if err != nil {
			return nil, m, cfgErr("%s: %s: %v", path, set.Label(), err)
		}
		set.Exclude = ex
		sets = append(sets, set)
	}
	if raw.Public != nil {
		set := ChatSet{Key: PublicChannelsKey, Public: true,
			Read: truthy(valueOr(raw.Public, "read", true)), Note: fmt.Sprint(valueOr(raw.Public, "note", ""))}
		if truthy(valueOr(raw.Public, "send", false)) {
			return nil, m, cfgErr("%s: [public_channels] — только чтение; писать в канал можно, "+
				"только открыв его отдельным [[chat]] с send = true", path)
		}
		ex, err := excludeList(raw.Public["exclude"])
		if err != nil {
			return nil, m, cfgErr("%s: [public_channels]: %v", path, err)
		}
		set.Exclude = ex
		sets = append(sets, set)
	}
	if raw.Manage != nil {
		m.Folders = truthy(valueOr(raw.Manage, "folders", false))
	}
	return sets, m, nil
}

func excludeList(v any) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, errors.New("exclude — список id или @username")
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, fmt.Sprint(e))
	}
	return out, nil
}

// ── состав групп (пишет служба) ──────────────────────────────────────────

// SetMember — чат, входящий в группу.
type SetMember struct {
	Peer     int64  `json:"peer"`
	Title    string `json:"title"`
	Username string `json:"username,omitempty"`
	Kind     string `json:"kind"`
}

// SetState — состав одной группы.
type SetState struct {
	Members []SetMember `json:"members"`
	Title   string      `json:"title,omitempty"` // название папки в Telegram
	Error   string      `json:"error,omitempty"` // папки нет и т.п.
}

// SetsState — data/chat_sets.json.
type SetsState struct {
	Updated time.Time           `json:"updated"`
	Sets    map[string]SetState `json:"sets"`
	// Aliases — alias чата из группы (ключ — id чата): не меняется между пересчётами
	Aliases map[string]string `json:"aliases"`
	// Blocked — чаты, попавшие в группу после действий агента (положил в
	// папку, достал из архива): доступа через группу они не дают
	Blocked map[string][]int64 `json:"blocked,omitempty"`
}

func SetsPath() string { return filepath.Join(DataPath(), "chat_sets.json") }

// LoadSetsState — что служба знает о составе групп (нет файла — пусто).
func LoadSetsState() SetsState {
	st := SetsState{}
	if raw, err := os.ReadFile(SetsPath()); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	if st.Sets == nil {
		st.Sets = map[string]SetState{}
	}
	if st.Aliases == nil {
		st.Aliases = map[string]string{}
	}
	if st.Blocked == nil {
		st.Blocked = map[string][]int64{}
	}
	return st
}

// SaveSetsState пишет состав групп атомарно.
func SaveSetsState(st SetsState) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	path := SetsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsx.WriteFile(path, raw, 0o600)
}

// IsBlocked — чат попал в группу из-за агента.
func (st SetsState) IsBlocked(key string, peer int64) bool {
	return slices.Contains(st.Blocked[key], peer)
}

// mergeSets дописывает к белому списку чаты из групп. Явный [[chat]] главнее:
// чат, описанный отдельно, из группы не берётся. Чат в нескольких группах
// получает права всех (отправку с автоотправкой — если её даёт одна группа).
func mergeSets(chats map[string]ChatRule, order []string, sets []ChatSet, st SetsState) (map[string]ChatRule, []string) {
	if len(sets) == 0 {
		return chats, order
	}
	if chats == nil {
		chats = map[string]ChatRule{}
	}
	explicitID := map[int64]bool{}
	explicitName := map[string]bool{}
	taken := map[string]bool{}
	for alias, r := range chats {
		taken[strings.ToLower(alias)] = true
		switch p := r.Peer.(type) {
		case int64:
			explicitID[p] = true
		case string:
			v := strings.TrimSpace(p)
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				explicitID[n] = true
			} else {
				explicitName[strings.ToLower(strings.TrimPrefix(v, "@"))] = true
			}
		}
	}
	type acc struct {
		rule   ChatRule
		labels []string
	}
	byPeer := map[int64]*acc{}
	var peers []int64
	for _, set := range sets {
		state, ok := st.Sets[set.Key]
		if !ok {
			continue
		}
		for _, m := range state.Members {
			if explicitID[m.Peer] || (m.Username != "" && explicitName[strings.ToLower(m.Username)]) ||
				set.Excluded(m.Peer, m.Username) || st.IsBlocked(set.Key, m.Peer) {
				continue
			}
			a := byPeer[m.Peer]
			if a == nil {
				alias := st.Aliases[strconv.FormatInt(m.Peer, 10)]
				if alias == "" || taken[strings.ToLower(alias)] {
					// alias ещё не выдан или занят явным чатом — до пересчёта службой
					alias = fmt.Sprintf("chat%d", absID(m.Peer))
				}
				taken[strings.ToLower(alias)] = true
				title := m.Title
				if title == "" {
					title = alias
				}
				a = &acc{rule: ChatRule{Alias: alias, Peer: m.Peer, Title: title, Note: set.Note}}
				byPeer[m.Peer] = a
				peers = append(peers, m.Peer)
			}
			a.rule.Read = a.rule.Read || set.Read
			if set.Send {
				a.rule.Send = true
				a.rule.Auto = a.rule.Auto || set.AutoSend()
			}
			a.labels = append(a.labels, set.Label())
		}
	}
	for _, p := range peers {
		a := byPeer[p]
		a.rule.Via = strings.Join(a.labels, ", ")
		chats[a.rule.Alias] = a.rule
		order = append(order, a.rule.Alias)
	}
	return chats, order
}

func absID(id int64) int64 {
	if id < 0 {
		return -id
	}
	return id
}
