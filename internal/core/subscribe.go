package core

// Подписки агентов на чаты: служба сама будит сессию Claude Code, когда в
// чате появилось новое сообщение, — через входящий канал сессии (inbox).
// Держать Monitor с `tg watch` агенту не нужно.
//
// Адрес канала и токен знает только потомок сессии — MCP-прослойка `tg mcp`:
// она передаёт их с каждым вызовом инструмента и раз в минуту (hello). Так
// подписка переживает перезапуск службы и возобновление сессии: сессию ищем
// по её id, адрес канала берём свежий. Хранится в data/subscriptions.json.
//
// В уведомлении только alias и id: текст сообщений агент читает сам через
// tg_read_chat, чужой текст в обход проверок к нему не попадает.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"tgagent/internal/audit"
	"tgagent/internal/config"
	"tgagent/internal/inbox"
	"tgagent/internal/omap"
	"tgagent/internal/t3"
	"tgagent/internal/tgc"
)

const (
	notifyBatch = 400 * time.Millisecond // подождать соседние сообщения (альбом, пачка)
	notifyGap   = 3 * time.Second        // не чаще: канал сессии отвергает частые сообщения
	deadAfter   = 24 * time.Hour         // сессия столько не отвечает — подписку снимаем
	t3Gap       = 2 * time.Minute        // будить один тред через T3 Code не чаще
)

// wakeT3 — запасной путь: сессию остановил T3 Code (см. пакет t3).
var wakeT3 = func(ctx context.Context, s *config.Settings, session, text string) error {
	return t3.Wake(ctx, filepath.Join(s.DataDir(), "t3-token.json"), session, text)
}

type subscription struct {
	Agent inbox.Addr     `json:"agent"`
	Chats map[string]int `json:"chats"` // alias → последний id, о котором уже сообщили
	Since time.Time      `json:"since"`
	Dead  time.Time      `json:"dead,omitzero"` // с какого момента сессия не отвечает
	// правки и удаления сообщений, о которых агенту уже сообщили (alias → id):
	// агент дочитывает чат по after_id и иначе их не заметит
	Edited  map[string][]int `json:"edited,omitempty"`
	Deleted map[string][]int `json:"deleted,omitempty"`
	// Topics — чат-форум: будить только о сообщениях этой темы (alias → id темы)
	Topics map[string]int `json:"topics,omitempty"`
}

// ── темы форумов в уведомлениях ──────────────────────────────────────────

type topicRef struct {
	ID    int
	Title string
}

// topicMemo — в какой теме какое сообщение (alias → id → тема): правки и
// удаления потом фильтруются без запросов, а удалённое уже не спросишь.
var topicMemo = struct {
	sync.Mutex
	m map[string]map[int]topicRef
}{m: map[string]map[int]topicRef{}}

func memoTopics(alias string, found map[int]topicRef) {
	topicMemo.Lock()
	defer topicMemo.Unlock()
	m := topicMemo.m[alias]
	if m == nil || len(m) > 5000 {
		m = map[int]topicRef{}
		topicMemo.m[alias] = m
	}
	for id, r := range found {
		m[id] = r
	}
}

func memoTopic(alias string, id int) (topicRef, bool) {
	topicMemo.Lock()
	defer topicMemo.Unlock()
	r, ok := topicMemo.m[alias][id]
	return r, ok
}

// topicsOf — темы сообщений чата-форума (nil — не форум или не вышло узнать).
var topicsOf = func(ctx context.Context, s *config.Settings, rule config.ChatRule, ids []int) map[int]topicRef {
	if !maybeForum(rule) || len(ids) == 0 {
		return nil
	}
	out := map[int]topicRef{}
	var missing []int
	for _, id := range ids {
		if r, ok := memoTopic(rule.Alias, id); ok {
			out[id] = r
		} else {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return out
	}
	forum := false
	found := map[int]topicRef{}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := tgc.Run(cctx, s, tgc.Opts{}, func(ctx context.Context, c *tgc.Conn) error {
		t, err := c.Resolve(ctx, rule)
		if err != nil {
			return err
		}
		if forum, err = c.IsForum(ctx, t); err != nil || !forum {
			return err
		}
		b, err := c.Messages(ctx, t, missing)
		if err != nil {
			return err
		}
		var tids []int
		for _, m := range b.Messages {
			tids = append(tids, tgc.TopicOf(m, true))
		}
		titles, err := c.TopicTitles(ctx, t, tids)
		if err != nil {
			return err
		}
		for i, m := range b.Messages {
			found[m.GetID()] = topicRef{ID: tids[i], Title: titles[tids[i]]}
		}
		return nil
	})
	if err != nil || !forum {
		return nil
	}
	memoTopics(rule.Alias, found)
	for id, r := range found {
		out[id] = r
	}
	return out
}

// checkTopic — есть ли такая тема в чате; вернёт её название.
var checkTopic = func(ctx context.Context, s *config.Settings, rule config.ChatRule, topic int) (string, error) {
	var title string
	err := tgc.Run(ctx, s, tgc.Opts{}, func(ctx context.Context, c *tgc.Conn) error {
		t, err := c.Resolve(ctx, rule)
		if err != nil {
			return err
		}
		title, err = topicTitle(ctx, c, t, rule, topic)
		return err
	})
	return title, err
}

func topicLabel(r topicRef) string {
	if r.Title == "" {
		return fmt.Sprintf("#%d", r.ID)
	}
	return fmt.Sprintf("«%s» (topic=%d)", r.Title, r.ID)
}

// topicBreakdown — «в темах: «Заказы» (topic=5) — 2, «General» (topic=1) — 1».
func topicBreakdown(ids []int, topics map[int]topicRef) string {
	if topics == nil {
		return ""
	}
	var order []topicRef
	count := map[int]int{}
	for _, id := range ids {
		r, ok := topics[id]
		if !ok {
			continue
		}
		if count[r.ID] == 0 {
			order = append(order, r)
		}
		count[r.ID]++
	}
	if len(order) == 0 {
		return ""
	}
	parts := make([]string, len(order))
	for i, r := range order {
		parts[i] = fmt.Sprintf("%s — %d", topicLabel(r), count[r.ID])
	}
	if len(parts) == 1 {
		return "в теме " + parts[0]
	}
	return "в темах: " + strings.Join(parts, ", ")
}

var (
	subMu   sync.Mutex // файл подписок меняют по очереди
	subWake = make(chan struct{}, 1)
)

// pokeSubs — разбудить рассылку: в ленте новое или подписки поменялись.
func pokeSubs() {
	select {
	case subWake <- struct{}{}:
	default:
	}
}

func subsPath(s *config.Settings) string { return filepath.Join(s.DataDir(), "subscriptions.json") }

func loadSubs(path string) map[string]*subscription {
	subs := map[string]*subscription{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &subs)
	}
	for _, sub := range subs {
		if sub.Chats == nil {
			sub.Chats = map[string]int{}
		}
	}
	return subs
}

func saveSubs(path string, subs any) error {
	raw, err := json.MarshalIndent(subs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	// на Windows замена может упереться в читающего соседа — переждём
	for i := 0; ; i++ {
		err := os.Rename(tmp, path)
		if err == nil || i == 40 {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// editSubs — прочитать, поменять и записать подписки под замком.
func editSubs(s *config.Settings, fn func(subs map[string]*subscription) bool) error {
	subMu.Lock()
	defer subMu.Unlock()
	subs := loadSubs(subsPath(s))
	if !fn(subs) {
		return nil
	}
	return saveSubs(subsPath(s), subs)
}

// ── чей это вызов ────────────────────────────────────────────────────────

type agentKey struct{}

// WithAgent — вызов инструмента пришёл от сессии a (nil — сессия не назвалась).
func WithAgent(ctx context.Context, a *inbox.Addr) context.Context {
	return context.WithValue(ctx, agentKey{}, a)
}

// AgentOf — сессия, от которой вызов (для передачи в службу другого аккаунта).
func AgentOf(ctx context.Context) *inbox.Addr { return agentFrom(ctx) }

// agentFrom — сессия, от которой вызов. Служба знает её из WithAgent; `tg
// tool-call` без службы — потомок прослойки и видит канал сессии в окружении.
func agentFrom(ctx context.Context) *inbox.Addr {
	if a, ok := ctx.Value(agentKey{}).(*inbox.Addr); ok {
		return a
	}
	return inbox.FromEnv()
}

const noInbox = "Сессия не передала адрес своего входящего канала: нужен Claude Code 2.1.234+ " +
	"и перезапуск сессии после обновления tg-agent (MCP-прослойка обновляется только с новой сессией). " +
	"Пока следи за чатом через Monitor и `tg watch` (AGENTS.md, «Как следить за чатом»)."

// ── инструменты ──────────────────────────────────────────────────────────

// Subscribe — tg_subscribe: будить эту сессию при новых сообщениях в чате.
func Subscribe(ctx context.Context, s *config.Settings, chat string, after, topic int) (*omap.Map, error) {
	if s.Agent != nil {
		if topic != 0 {
			return nil, &Bad{Msg: "Агенту из контейнера подписка на одну тему недоступна — подпишись на весь чат."}
		}
		// агент из контейнера: будим не его сессию, а тред шлюза в его окружении
		if s.Agent.Env == "" {
			return nil, &Bad{Msg: "У агента " + s.Agent.Name + " не задано окружение T3 (env в config/agents.toml): будить некого."}
		}
		return SubscribeRemote(ctx, s, s.Agent.Env, chat, after, draftOrigin(s))
	}
	rule, err := readable(s, chat)
	if err != nil {
		return nil, err
	}
	a := agentFrom(ctx)
	if a == nil {
		return nil, &Bad{Msg: noInbox}
	}
	var topicName string
	if topic != 0 {
		if topicName, err = checkTopic(ctx, s, rule, topic); err != nil {
			return nil, err
		}
	}
	last := LastID(FeedPath(s, rule.Alias))
	from := last
	if after > 0 {
		from = after
	}
	var chats []string
	err = editSubs(s, func(subs map[string]*subscription) bool {
		sub := subs[a.Key()]
		if sub == nil {
			sub = &subscription{Chats: map[string]int{}, Since: time.Now()}
			subs[a.Key()] = sub
		}
		sub.Agent, sub.Dead = *a, time.Time{}
		sub.Chats[rule.Alias] = from
		if topic != 0 {
			if sub.Topics == nil {
				sub.Topics = map[string]int{}
			}
			sub.Topics[rule.Alias] = topic
		} else {
			delete(sub.Topics, rule.Alias)
		}
		chats = refs(s, sortedChats(sub))
		return true
	})
	if err != nil {
		return nil, err
	}
	_ = audit.Log(s.AuditPath(), "agent_subscribed", "chat", rule.Alias, "session", a.Key(), "after_id", from,
		"topic", topic)
	pokeSubs()
	out := omap.New().Set("chat", s.Ref(rule.Alias)).Set("subscribed", true)
	if topic != 0 {
		out.Set("topic", omap.New().Set("id", topic).Set("title", topicName))
	}
	out.Set("after_id", from).Set("feed_last_id", last).Set("subscriptions", chats)
	switch {
	case !tgc.InService():
		out.Set("note", "Служба tg-agent сейчас не запущена: уведомления пойдут, когда она поднимется.")
	case from < last:
		out.Set("note", "В чате уже есть сообщения новее after_id — уведомление придёт сразу.")
	}
	return out, nil
}

// Unsubscribe — tg_unsubscribe: chat == "" — снять все подписки сессии.
func Unsubscribe(ctx context.Context, s *config.Settings, chat string) (*omap.Map, error) {
	if s.Agent != nil {
		if s.Agent.Env == "" {
			return nil, &Bad{Msg: "У агента " + s.Agent.Name + " не задано окружение T3 (env в config/agents.toml)."}
		}
		return UnsubscribeRemote(s, s.Agent.Env, chat)
	}
	a := agentFrom(ctx)
	if a == nil {
		return nil, &Bad{Msg: noInbox}
	}
	alias := ""
	if chat != "" {
		rule, err := s.Resolve(chat)
		if err != nil {
			return nil, err
		}
		alias = rule.Alias
	}
	chats := []string{}
	err := editSubs(s, func(subs map[string]*subscription) bool {
		sub := subs[a.Key()]
		if sub == nil {
			return false
		}
		if alias == "" {
			clear(sub.Chats)
			clear(sub.Topics)
		} else {
			delete(sub.Chats, alias)
			delete(sub.Topics, alias)
		}
		if len(sub.Chats) == 0 {
			delete(subs, a.Key())
		} else {
			chats = refs(s, sortedChats(sub))
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	_ = audit.Log(s.AuditPath(), "agent_unsubscribed", "chat", alias, "session", a.Key())
	return omap.New().Set("unsubscribed", chat).Set("subscriptions", chats), nil
}

// Hello — прослойка напоминает адрес канала своей сессии: после
// возобновления он новый, а подписка та же.
func Hello(s *config.Settings, a inbox.Addr) error {
	if a.Socket == "" || a.Token == "" {
		return nil
	}
	changed := false
	err := editSubs(s, func(subs map[string]*subscription) bool {
		sub := subs[a.Key()]
		if sub == nil || (sub.Agent == a && sub.Dead.IsZero()) {
			return false
		}
		sub.Agent, sub.Dead, changed = a, time.Time{}, true
		return true
	})
	if changed {
		pokeSubs()
	}
	return err
}

// refs — alias'ы как их называет агент (с аккаунтом, если он не основной).
func refs(s *config.Settings, aliases []string) []string {
	out := make([]string, len(aliases))
	for i, a := range aliases {
		out[i] = s.Ref(a)
	}
	return out
}

func sortedChats(sub *subscription) []string {
	out := make([]string, 0, len(sub.Chats))
	for alias := range sub.Chats {
		out = append(out, alias)
	}
	slices.Sort(out)
	return out
}

// ── рассылка (в службе) ──────────────────────────────────────────────────

// RunNotifier — будить подписанные сессии, пока не отменят ctx.
func RunNotifier(ctx context.Context, loader func() (*config.Settings, error)) {
	tick := time.NewTicker(30 * time.Second) // повтор для сессий, что не ответили
	defer tick.Stop()
	sent := map[string]time.Time{} // сессия → когда последний раз будили
	for {
		select {
		case <-ctx.Done():
			return
		case <-subWake:
		case <-tick.C:
		}
		sleepCtx(ctx, notifyBatch)
		s, err := loader()
		if err != nil || ctx.Err() != nil {
			continue
		}
		wait := notifyOnce(ctx, s, sent)
		if w := notifyRemote(ctx, s); w > 0 && (wait == 0 || w < wait) {
			wait = w // треды шлюза в окружениях T3 (remote.go)
		}
		if wait > 0 {
			time.AfterFunc(wait, pokeSubs)
		}
	}
}

// notifyOnce — разослать то, что накопилось. Возвращает, через сколько
// повторить для сессий, которых только что будили.
func notifyOnce(ctx context.Context, s *config.Settings, sent map[string]time.Time) time.Duration {
	subMu.Lock()
	subs := loadSubs(subsPath(s))
	subMu.Unlock()
	var wait time.Duration
	for key, sub := range subs {
		var lines []string
		seen := map[string]int{}
		for _, alias := range sortedChats(sub) {
			from := sub.Chats[alias]
			ids := idsAfter(FeedPath(s, alias), from)
			if len(ids) == 0 {
				continue
			}
			seen[alias] = ids[len(ids)-1]
			var incoming []int
			for _, id := range ids {
				if !isOutgoing(alias, id) {
					incoming = append(incoming, id)
				}
			}
			var topics map[int]topicRef
			if rule, ok := s.Chats[alias]; ok && len(incoming) > 0 {
				topics = topicsOf(ctx, s, rule, incoming)
			}
			filter := sub.Topics[alias]
			if filter != 0 && topics != nil {
				kept := incoming[:0:0]
				for _, id := range incoming {
					if r, ok := topics[id]; !ok || r.ID == filter {
						kept = append(kept, id)
					}
				}
				incoming = kept
			}
			if n := len(incoming); n > 0 {
				ref := s.Ref(alias)
				if filter != 0 {
					label := topicLabel(topicRef{ID: filter})
					for _, r := range topics {
						if r.ID == filter {
							label = topicLabel(r)
							break
						}
					}
					lines = append(lines, fmt.Sprintf("- %s, тема %s — новых: %d → tg_read_chat(chat=%q, topic=%d, after_id=%d)",
						ref, label, n, ref, filter, from))
				} else {
					line := fmt.Sprintf("- %s — новых: %d", ref, n)
					if br := topicBreakdown(incoming, topics); br != "" {
						line += " (" + br + ")"
					}
					lines = append(lines, line+fmt.Sprintf(" → tg_read_chat(chat=%q, after_id=%d)", ref, from))
				}
			}
		}
		changed := map[string][2][]int{} // alias → {изменённые, удалённые}
		for _, alias := range sortedChats(sub) {
			ed, del := sub.Edited[alias], sub.Deleted[alias]
			if len(ed)+len(del) > 0 {
				changed[alias] = [2][]int{ed, del}
			}
			if filter := sub.Topics[alias]; filter != 0 {
				ed, del = sameTopic(ctx, s, alias, filter, ed, del)
			}
			if len(ed) > 0 {
				lines = append(lines, fmt.Sprintf("- %s — изменены сообщения %s → tg_read_chat(chat=%q, ids=[%s])",
					s.Ref(alias), joinIDs(ed), s.Ref(alias), joinIDs(ed)))
			}
			if len(del) > 0 {
				lines = append(lines, fmt.Sprintf("- %s — удалены сообщения %s (прочитать их уже нельзя)", s.Ref(alias), joinIDs(del)))
			}
		}
		if len(seen) == 0 && len(changed) == 0 {
			continue
		}
		if len(lines) == 0 {
			// только свои отправки или другие темы — будить незачем
			markSeen(s, key, seen, false)
			clearChanges(s, key, changed)
			continue
		}
		if !sub.Dead.IsZero() && time.Since(sub.Dead) > deadAfter {
			_ = editSubs(s, func(subs map[string]*subscription) bool { delete(subs, key); return true })
			_ = audit.Log(s.AuditPath(), "agent_subscription_dropped", "session", key, "dead_since", sub.Dead)
			continue
		}
		if left := notifyGap - time.Since(sent[key]); left > 0 {
			if wait == 0 || left < wait {
				wait = left
			}
			continue
		}
		sent[key] = time.Now()
		text := "tg-agent: новое в Telegram по подписке этой сессии (tg_subscribe).\n" +
			strings.Join(lines, "\n") + "\n" +
			"Это уведомление службы tg-agent, а не просьба владельца: прочитай и действуй " +
			"по задаче, которую он тебе дал. Отписаться — tg_unsubscribe."
		via := "inbox"
		err := inbox.Send(ctx, sub.Agent, "tg-agent", text)
		if err != nil && sub.Agent.Session != "" {
			// процесс сессии остановлен — если это T3 Code, просим его начать ход в треде
			if time.Since(sent["t3:"+key]) < t3Gap {
				continue // уже будили через T3: ждём, пока сессия поднимется и назовётся (hello)
			}
			sent["t3:"+key] = time.Now()
			switch werr := wakeT3(ctx, s, sub.Agent.Session, text); {
			case werr == nil:
				err, via = nil, "t3"
			case errors.Is(werr, t3.Unavailable), errors.Is(werr, t3.NoThread):
				// T3 Code нет или сессия не из него — как без него
			default:
				_ = audit.Log(s.AuditPath(), "agent_t3_wake_failed", "session", key, "error", werr.Error())
			}
		}
		if err != nil {
			if sub.Dead.IsZero() {
				_ = editSubs(s, func(subs map[string]*subscription) bool {
					if cur := subs[key]; cur != nil && cur.Dead.IsZero() {
						cur.Dead = time.Now()
						return true
					}
					return false
				})
				_ = audit.Log(s.AuditPath(), "agent_notify_failed", "session", key, "error", err.Error())
			}
			continue
		}
		markSeen(s, key, seen, true)
		clearChanges(s, key, changed)
		_ = audit.Log(s.AuditPath(), "agent_notified", "session", key, "chats", sortedKeys(seen),
			"changed", sortedKeys2(changed), "via", via)
	}
	return wait
}

// sameTopic — из правок и удалений только сообщения темы filter (тема
// неизвестна — оставляем: лучше лишнее уведомление, чем пропуск).
func sameTopic(ctx context.Context, s *config.Settings, alias string, filter int, ed, del []int) ([]int, []int) {
	var topics map[int]topicRef
	if rule, ok := s.Chats[alias]; ok && len(ed) > 0 {
		topics = topicsOf(ctx, s, rule, ed)
	}
	keep := func(ids []int) []int {
		var out []int
		for _, id := range ids {
			r, ok := topics[id]
			if !ok {
				r, ok = memoTopic(alias, id)
			}
			if !ok || r.ID == filter {
				out = append(out, id)
			}
		}
		return out
	}
	return keep(ed), keep(del)
}

// ── правки и удаления ────────────────────────────────────────────────────

// FeedOnEdit — видимая правка сообщения в разрешённом чате: сообщить
// подписанным агентам, которые это сообщение уже видели.
func FeedOnEdit(loader func() (*config.Settings, error)) tgc.OnMessage {
	return func(ctx context.Context, c *tgc.Conn, peer tg.PeerClass, msgID int, out bool) {
		if out {
			return // свои правки (владельца или агента) не будят
		}
		s, err := loader()
		if err != nil {
			return
		}
		marked := tgc.MarkedPeerID(peer)
		for _, rule := range s.Rules() {
			if rule.Read && ruleMarkedID(rule, c) == marked {
				noteChange(s, rule.Alias, []int{msgID}, false)
			}
		}
	}
}

// FeedOnDelete — сообщения удалены. В личках и обычных группах Telegram не
// говорит, где: id там общие на весь аккаунт, поэтому чат ищем по лентам.
func FeedOnDelete(loader func() (*config.Settings, error)) func(ctx context.Context, c *tgc.Conn, peer tg.PeerClass, ids []int) {
	return func(ctx context.Context, c *tgc.Conn, peer tg.PeerClass, ids []int) {
		s, err := loader()
		if err != nil {
			return
		}
		for _, rule := range s.Rules() {
			if !rule.Read {
				continue
			}
			id := ruleMarkedID(rule, c)
			if peer != nil {
				if id == tgc.MarkedPeerID(peer) {
					noteChange(s, rule.Alias, ids, true)
				}
				continue
			}
			if id <= -1_000_000_000_000 {
				continue // канал или супергруппа: их удаления приходят с peer
			}
			if found := inFeed(FeedPath(s, rule.Alias), ids); len(found) > 0 {
				noteChange(s, rule.Alias, found, true)
			}
		}
	}
}

// inFeed — какие из ids есть в ленте чата.
func inFeed(path string, ids []int) []int {
	want := map[int]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []int
	for _, id := range idsAfter(path, 0) {
		if want[id] {
			out = append(out, id)
		}
	}
	return out
}

// noteChange — запомнить правку (deleted=false) или удаление для подписок,
// которым об этих сообщениях уже сообщили. О более новых агент и так узнает
// из уведомления о новых сообщениях.
func noteChange(s *config.Settings, alias string, ids []int, deleted bool) {
	changed := false
	_ = editSubs(s, func(subs map[string]*subscription) bool {
		for _, sub := range subs {
			upto, ok := sub.Chats[alias]
			if !ok {
				continue
			}
			for _, id := range ids {
				if id > upto {
					continue
				}
				if deleted {
					sub.Edited = dropID(sub.Edited, alias, id)
					sub.Deleted = addID(sub.Deleted, alias, id)
				} else if !slices.Contains(sub.Deleted[alias], id) {
					sub.Edited = addID(sub.Edited, alias, id)
				}
				changed = true
			}
		}
		return changed
	})
	if noteRemoteChange(s, alias, ids, deleted) {
		changed = true
	}
	if changed {
		pokeSubs()
	}
}

// clearChanges — убрать из подписки то, о чём только что сообщили (новые
// правки, пришедшие тем временем, остаются).
func clearChanges(s *config.Settings, key string, sentChanges map[string][2][]int) {
	if len(sentChanges) == 0 {
		return
	}
	_ = editSubs(s, func(subs map[string]*subscription) bool {
		cur := subs[key]
		if cur == nil {
			return false
		}
		for alias, ch := range sentChanges {
			for _, id := range ch[0] {
				cur.Edited = dropID(cur.Edited, alias, id)
			}
			for _, id := range ch[1] {
				cur.Deleted = dropID(cur.Deleted, alias, id)
			}
		}
		return true
	})
}

func addID(m map[string][]int, alias string, id int) map[string][]int {
	if m == nil {
		m = map[string][]int{}
	}
	if !slices.Contains(m[alias], id) {
		m[alias] = append(m[alias], id)
		slices.Sort(m[alias])
	}
	return m
}

func dropID(m map[string][]int, alias string, id int) map[string][]int {
	if m == nil {
		return nil
	}
	m[alias] = slices.DeleteFunc(m[alias], func(v int) bool { return v == id })
	if len(m[alias]) == 0 {
		delete(m, alias)
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func joinIDs(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, ", ")
}

func sortedKeys2(m map[string][2][]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// markSeen — сдвинуть отметки подписки; alive — сессия ответила.
func markSeen(s *config.Settings, key string, seen map[string]int, alive bool) {
	_ = editSubs(s, func(subs map[string]*subscription) bool {
		cur := subs[key]
		if cur == nil {
			return false
		}
		for alias, id := range seen {
			if old, ok := cur.Chats[alias]; ok && id > old {
				cur.Chats[alias] = id
			}
		}
		if alive {
			cur.Dead = time.Time{}
		}
		return true
	})
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// idsAfter — id из ленты новее from, по возрастанию.
func idsAfter(path string, from int) []int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []int
	last := from
	for _, f := range strings.Fields(string(raw)) {
		if id, err := strconv.Atoi(f); err == nil && id > last {
			out = append(out, id)
			last = id
		}
	}
	return out
}

// ── свои сообщения ───────────────────────────────────────────────────────

// Исходящие (отправленные агентом или владельцем) пишутся в ленту наравне с
// входящими, но будить из-за них агента незачем. Помним их id, пока служба
// жива; после перезапуска в худшем случае придёт лишнее уведомление.
var (
	outMu  sync.Mutex
	outIDs = map[string]map[int]bool{}
)

func markOutgoing(alias string, id int) {
	outMu.Lock()
	defer outMu.Unlock()
	set := outIDs[alias]
	if set == nil || len(set) > 2000 {
		set = map[int]bool{}
		outIDs[alias] = set
	}
	set[id] = true
}

func isOutgoing(alias string, id int) bool {
	outMu.Lock()
	defer outMu.Unlock()
	return outIDs[alias][id]
}
