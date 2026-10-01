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
func Subscribe(ctx context.Context, s *config.Settings, chat string, after int) (*omap.Map, error) {
	if s.Agent != nil {
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
		chats = sortedChats(sub)
		return true
	})
	if err != nil {
		return nil, err
	}
	_ = audit.Log(s.AuditPath(), "agent_subscribed", "chat", rule.Alias, "session", a.Key(), "after_id", from)
	pokeSubs()
	out := omap.New().Set("chat", rule.Alias).Set("subscribed", true).
		Set("after_id", from).Set("feed_last_id", last).Set("subscriptions", chats)
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
		} else {
			delete(sub.Chats, alias)
		}
		if len(sub.Chats) == 0 {
			delete(subs, a.Key())
		} else {
			chats = sortedChats(sub)
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
			n := 0
			for _, id := range ids {
				if !isOutgoing(alias, id) {
					n++
				}
			}
			if n > 0 {
				lines = append(lines, fmt.Sprintf("- %s — новых: %d → tg_read_chat(chat=%q, after_id=%d)", alias, n, alias, from))
			}
		}
		if len(seen) == 0 {
			continue
		}
		if len(lines) == 0 {
			// только свои отправки — будить незачем
			markSeen(s, key, seen, false)
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
		text := "tg-agent: новые сообщения в Telegram по подписке этой сессии (tg_subscribe).\n" +
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
		_ = audit.Log(s.AuditPath(), "agent_notified", "session", key, "chats", sortedKeys(seen), "via", via)
	}
	return wait
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
