package core

// Состав групп белого списка ([[folder]], [public_channels] в chats.toml).
//
// Владелец открывает папку или все публичные каналы одним правилом, а кто в
// группе сейчас, служба пересчитывает сама (раз в 10 минут и сразу, как
// правило появилось): data/state.db. config.Load дописывает эти чаты к
// белому списку с правами из правила — снятое правило закрывает их сразу.

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/maxc"
	"tgagent/internal/tgc"
)

const setsEvery = 10 * time.Minute

var setsMu sync.Mutex

// RunSets — служба: держать состав групп свежим.
func RunSets(ctx context.Context, loader func() (*config.Settings, error)) {
	for ctx.Err() == nil {
		if s, err := loader(); err == nil {
			if changed, _ := RefreshSets(ctx, s, false); changed {
				setsChanged(ctx, loader)
			}
		}
		sleepCtx(ctx, time.Minute)
	}
}

// setsChanged — новые чаты групп сразу получают ленты.
func setsChanged(ctx context.Context, loader func() (*config.Settings, error)) {
	s, err := loader()
	if err != nil {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if n := netOf(s); n != nil {
		n.catchup(pctx, s)
	} else {
		_, _ = PollOnce(pctx, s)
	}
}

// RefreshSets — пересчитать состав групп, если он устарел (force — сейчас).
// changed — состав какой-то группы поменялся.
func RefreshSets(ctx context.Context, s *config.Settings, force bool) (bool, error) {
	setsMu.Lock()
	defer setsMu.Unlock()
	st := config.LoadSetsState()
	if len(s.Sets) == 0 {
		if len(st.Sets) == 0 {
			return false, nil
		}
		st.Sets = map[string]config.SetState{}
		return true, config.SaveSetMembers(st)
	}
	stale := force || time.Since(st.Updated) > setsEvery
	for _, set := range s.Sets {
		if _, ok := st.Sets[set.Key]; !ok {
			stale = true
		}
	}
	if !stale {
		return false, nil
	}
	var (
		fresh map[string]config.SetState
		err   error
	)
	if n := netOf(s); n != nil {
		fresh, err = n.setMembers(ctx, s)
	} else {
		fresh, err = tgSetMembers(ctx, s)
	}
	if err != nil {
		logEvent(s, "chat_sets_failed", "error", err.Error())
		return false, err
	}
	assignSetAliases(s, &st, fresh)
	changed := false
	for _, set := range s.Sets {
		was, now := st.Sets[set.Key], fresh[set.Key]
		added, removed := memberDiff(was.Members, now.Members)
		if len(added) == 0 && len(removed) == 0 && was.Error == now.Error {
			continue
		}
		changed = true
		kv := []any{"set", set.Key, "label", set.Label(), "count", len(now.Members),
			"added", titles(added, 20), "removed", titles(removed, 20)}
		if now.Error != "" {
			kv = append(kv, "error", now.Error)
		}
		logEvent(s, "chat_set_changed", kv...)
	}
	st.Sets, st.Updated = fresh, time.Now()
	return changed, config.SaveSetMembers(st)
}

func tgSetMembers(ctx context.Context, s *config.Settings) (map[string]config.SetState, error) {
	out := map[string]config.SetState{}
	err := tgc.Run(ctx, s, tgc.Opts{Wait: 30 * time.Second, Caller: "chat_sets"}, func(ctx context.Context, c *tgc.Conn) error {
		page, err := c.Dialogs(ctx)
		if err != nil {
			return err
		}
		var folders []tgc.Folder
		if slices.ContainsFunc(s.Sets, func(set config.ChatSet) bool { return !set.Public }) {
			if folders, err = c.Folders(ctx); err != nil {
				return err
			}
		}
		for _, set := range s.Sets {
			if set.Public {
				out[set.Key] = config.SetState{Members: members(c.PublicChannels(page))}
				continue
			}
			f, ok := findTGFolder(folders, set.Folder, set.Name)
			if !ok {
				out[set.Key] = config.SetState{Error: "папки нет среди папок аккаунта", Members: []config.SetMember{}}
				continue
			}
			out[set.Key] = config.SetState{Title: f.Title, Members: members(c.FolderRows(page, f))}
		}
		return nil
	})
	return out, err
}

func findTGFolder(folders []tgc.Folder, id, name string) (tgc.Folder, bool) {
	for _, f := range folders {
		if id != "" && strconv.Itoa(f.ID) == id {
			return f, true
		}
	}
	if id == "" {
		for _, f := range folders {
			if strings.EqualFold(f.Title, strings.TrimSpace(name)) {
				return f, true
			}
		}
	}
	return tgc.Folder{}, false
}

func maxSetMembers(ctx context.Context, s *config.Settings) (map[string]config.SetState, error) {
	c, err := maxConn(ctx)
	if err != nil {
		return nil, err
	}
	folders, err := c.Folders(ctx)
	if err != nil {
		return nil, err
	}
	all, err := c.AllChats(ctx, 1000)
	if err != nil {
		return nil, err
	}
	maxRT.Lock()
	self, contacts := maxRT.self, maxRT.contacts
	maxRT.Unlock()
	out := map[string]config.SetState{}
	for _, set := range s.Sets {
		if set.Public {
			out[set.Key] = config.SetState{Error: "в MAX у каналов нет публичных @username — открой папку", Members: []config.SetMember{}}
			continue
		}
		var found *maxc.Folder
		for i, f := range folders {
			if (set.Folder != "" && f.ID == set.Folder) || (set.Folder == "" && strings.EqualFold(f.Title, set.Name)) {
				found = &folders[i]
				break
			}
		}
		if found == nil {
			out[set.Key] = config.SetState{Error: "папки нет среди папок аккаунта", Members: []config.SetMember{}}
			continue
		}
		state := config.SetState{Title: found.Title, Members: []config.SetMember{}}
		for _, ch := range all {
			if found.Matches(ch.ID, maxc.ChatFlags(ch, self, contacts)) {
				state.Members = append(state.Members, config.SetMember{Peer: ch.ID, Title: maxChatTitle(ctx, c, ch), Kind: maxKind(ch.Type)})
			}
		}
		out[set.Key] = state
	}
	return out, nil
}

func members(rows []tgc.DialogRow) []config.SetMember {
	out := make([]config.SetMember, 0, len(rows))
	for _, r := range rows {
		out = append(out, config.SetMember{Peer: r.ID, Title: r.Title, Username: r.Username, Kind: r.Kind})
	}
	return out
}

// assignSetAliases — alias чатам групп: прежний, пока он не занят явным
// чатом; новый — из @username или названия.
func assignSetAliases(s *config.Settings, st *config.SetsState, fresh map[string]config.SetState) {
	used := map[string]int64{} // alias → чат
	for alias, r := range s.Chats {
		if r.Via == "" {
			used[strings.ToLower(alias)] = 0
		}
	}
	owner := func(alias string, peer int64) bool {
		p, ok := used[strings.ToLower(alias)]
		return !ok || p == peer
	}
	var pending []config.SetMember
	for _, set := range s.Sets {
		for _, m := range fresh[set.Key].Members {
			key := strconv.FormatInt(m.Peer, 10)
			if a := st.Aliases[key]; a != "" && owner(a, m.Peer) {
				used[strings.ToLower(a)] = m.Peer
				continue
			}
			pending = append(pending, m)
		}
	}
	for _, m := range pending {
		key := strconv.FormatInt(m.Peer, 10)
		if a := st.Aliases[key]; a != "" && used[strings.ToLower(a)] == m.Peer {
			continue // уже выдан (чат в двух группах)
		}
		base := aliasFromUsername(m.Username)
		if base == "" {
			base = slug(m.Title, m.Kind)
		}
		alias := base
		for i := 2; !owner(alias, m.Peer); i++ {
			alias = fmt.Sprintf("%s-%d", base, i)
		}
		used[strings.ToLower(alias)] = m.Peer
		st.Aliases[key] = alias
	}
}

func aliasFromUsername(u string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(u) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func memberDiff(was, now []config.SetMember) (added, removed []config.SetMember) {
	in := func(list []config.SetMember, peer int64) bool {
		return slices.ContainsFunc(list, func(m config.SetMember) bool { return m.Peer == peer })
	}
	for _, m := range now {
		if !in(was, m.Peer) {
			added = append(added, m)
		}
	}
	for _, m := range was {
		if !in(now, m.Peer) {
			removed = append(removed, m)
		}
	}
	return added, removed
}

func titles(list []config.SetMember, limit int) []string {
	out := []string{}
	for i, m := range list {
		if i == limit {
			out = append(out, fmt.Sprintf("… ещё %d", len(list)-limit))
			break
		}
		out = append(out, m.Title)
	}
	return out
}

// blockForAgent — агент сейчас переложит чат (папка, архив): в группы, где
// чата нет, он после этого попасть не должен — иначе раскладка по папкам
// открыла бы агенту чат, который владелец не открывал.
func blockForAgent(s *config.Settings, peer int64) error {
	if len(s.Sets) == 0 {
		return nil
	}
	// блоки пишутся отдельно от состава (BlockSetChat): пересчёт групп,
	// идущий в это время, их не затрёт
	st := config.LoadSetsState()
	for _, set := range s.Sets {
		state := st.Sets[set.Key]
		member := slices.ContainsFunc(state.Members, func(m config.SetMember) bool { return m.Peer == peer })
		if !member && !st.IsBlocked(set.Key, peer) {
			if err := config.BlockSetChat(set.Key, peer); err != nil {
				return err
			}
		}
	}
	return nil
}

// clearBlocked — владелец заново открыл группу целиком: и чаты, которые туда
// положил агент, тоже.
// Сбой базы — retryable: решение владельца о группе без снятых блоков
// исполнено бы частью, и нажатие надо разобрать снова.
func clearBlocked(key string) error {
	if err := config.ClearSetBlocked(key); err != nil {
		return retryable{err}
	}
	return nil
}

// setOf — правило на группу по ключу.
func setOf(s *config.Settings, key string) (config.ChatSet, bool) {
	for _, set := range s.Sets {
		if set.Key == key {
			return set, true
		}
	}
	return config.ChatSet{}, false
}

// folderSet — правило на папку (по id или названию).
func folderSet(s *config.Settings, id, title string) (config.ChatSet, bool) {
	for _, set := range s.Sets {
		if set.Public {
			continue
		}
		if (set.Folder != "" && set.Folder == id) || (set.Folder == "" && strings.EqualFold(set.Name, title)) {
			return set, true
		}
	}
	return config.ChatSet{}, false
}

// ParseSince — граница «с какого времени»: длительность назад («24h»,
// «90m», «2d», «1w») или момент («2026-11-02», «2026-11-02 10:00», RFC3339).
// Без часового пояса — местное время.
func ParseSince(v string, now time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	if n := len(v); n > 1 && (v[n-1] == 'd' || v[n-1] == 'w') {
		if k, err := strconv.ParseFloat(v[:n-1], 64); err == nil && k > 0 {
			day := 24 * time.Hour
			if v[n-1] == 'w' {
				day *= 7
			}
			return now.Add(-time.Duration(k * float64(day))), nil
		}
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, &Bad{Msg: fmt.Sprintf("since «%s» не понять: нужно «24h», «2d» или дата «2026-11-02» / «2026-11-02T10:00».", v)}
}
