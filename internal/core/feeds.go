package core

// Ленты новых сообщений: таблица feed в data/state.db — id сообщений чата.
//
// Пишет служба: Telegram сам присылает ей новые сообщения (FeedOnMessage), а
// раз в минуту она сверяет верхние сообщения чатов (PollOnce) — на случай
// пропусков и собственных отправок. Читают подписки, tg watch и
// tg_list_chats (feed.last_id) — без сессии и сети.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"tgagent/internal/audit"
	"tgagent/internal/config"
	"tgagent/internal/tgc"
)

// FeedLast — последний id в ленте чата (0 — ленты ещё нет).
func FeedLast(s *config.Settings, alias string) int {
	db, err := stateDB(s)
	if err != nil {
		return 0
	}
	var last sql.NullInt64
	if err := db.Q().QueryRowContext(context.Background(), "SELECT max(msg_id) FROM feed WHERE chat = ?", alias).Scan(&last); err != nil {
		return 0
	}
	return int(last.Int64)
}

// idsAfter — id из ленты новее from, по возрастанию.
func idsAfter(s *config.Settings, alias string, from int) []int {
	db, err := stateDB(s)
	if err != nil {
		return nil
	}
	rows, err := db.Q().QueryContext(context.Background(),
		"SELECT msg_id FROM feed WHERE chat = ? AND msg_id > ? ORDER BY msg_id", alias, from)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// PollOnce — сверка: дописать во все ленты то, что пришло после последнего
// id. Возвращает, сколько добавлено.
func PollOnce(ctx context.Context, s *config.Settings) (map[string]int, error) {
	added := map[string]int{}
	var rules []config.ChatRule
	groups := groupPollDue()
	for _, r := range s.Rules() {
		// чаты групп (папка, сотни публичных каналов) сверяем реже: новое и
		// так приходит событиями, а сверка — только на случай пропусков
		if r.Read && (r.Via == "" || groups || FeedLast(s, r.Alias) == 0) {
			rules = append(rules, r)
		}
	}
	if len(rules) == 0 {
		return added, nil
	}
	err := tgc.Run(ctx, s, tgc.Opts{Wait: 30 * time.Second, Caller: "poll_once"},
		func(ctx context.Context, c *tgc.Conn) error {
			targets := map[string]tgc.Target{}
			byAlias := map[string]config.ChatRule{}
			var groupRules []config.ChatRule
			for _, r := range rules {
				byAlias[r.Alias] = r
				if _, ok := r.Peer.(int64); ok && r.Via != "" {
					groupRules = append(groupRules, r) // их верх — из списка диалогов
					continue
				}
				t, err := c.Resolve(ctx, r)
				if err != nil {
					return err
				}
				targets[r.Alias] = t
			}
			top, err := c.TopMessages(ctx, targets)
			if err != nil {
				return err
			}
			if len(groupRules) > 0 {
				tops, err := c.GroupTops(ctx, groupRules)
				if err != nil {
					return err
				}
				for alias, id := range tops {
					top[alias] = id
				}
			}
			for alias, topID := range top {
				known := FeedLast(s, alias)
				if known == 0 {
					// новая лента начинается с текущего момента — историю не льём
					if _, err := appendNewer(s, alias, []int{topID}); err != nil {
						return err
					}
					continue
				}
				if topID <= known {
					continue
				}
				t, ok := targets[alias]
				if !ok {
					if t, err = c.Resolve(ctx, byAlias[alias]); err != nil {
						return err
					}
				}
				batch, err := c.History(ctx, t, 200, 0, known, "", 0)
				if err != nil {
					return err
				}
				var ids []int
				for i := len(batch.Messages) - 1; i >= 0; i-- {
					m := batch.Messages[i]
					if id := m.GetID(); id > known {
						ids = append(ids, id)
						if o, ok := m.(interface{ GetOut() bool }); ok && o.GetOut() {
							markOutgoing(alias, id)
						}
					}
				}
				n, err := appendNewer(s, alias, ids)
				if err != nil {
					return err
				}
				if n > 0 {
					added[alias] = n
				}
			}
			return nil
		})
	return added, err
}

// groupPollEvery — как часто сверка проходит по чатам групп.
const groupPollEvery = 10 * time.Minute

var groupPoll struct {
	sync.Mutex
	last time.Time
}

func groupPollDue() bool {
	groupPoll.Lock()
	defer groupPoll.Unlock()
	if time.Since(groupPoll.last) < groupPollEvery {
		return false
	}
	groupPoll.last = time.Now()
	return true
}

// appendNewer дописывает в ленту id новее последнего в ней. Новая лента
// начинается с текущего сообщения — историю не льём. Проверка и запись —
// одной транзакцией: ленту дописывают и события, и сверка.
func appendNewer(s *config.Settings, alias string, ids []int) (int, error) {
	db, err := stateDB(s)
	if err != nil {
		return 0, err
	}
	ctx := context.Background()
	n := 0
	err = db.Write(ctx, func(tx *sql.Tx) error {
		n = 0
		var last sql.NullInt64
		if err := tx.QueryRowContext(ctx, "SELECT max(msg_id) FROM feed WHERE chat = ?", alias).Scan(&last); err != nil {
			return err
		}
		top := int(last.Int64)
		for _, id := range ids {
			if id <= top {
				continue
			}
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO feed(chat, msg_id) VALUES(?, ?)", alias, id); err != nil {
				return err
			}
			top = id
			n++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if n > 0 {
		pokeSubs()
	}
	return n, nil
}

// ruleMarkedID — id чата из белого списка в формате Bot API (0 — пока неизвестен).
func ruleMarkedID(rule config.ChatRule, c *tgc.Conn) int64 {
	switch p := rule.Peer.(type) {
	case int64:
		return p
	case string:
		v := strings.TrimSpace(p)
		if strings.EqualFold(v, "me") || strings.EqualFold(v, "self") {
			return c.Peers.SelfID()
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
		return c.Peers.MarkedByUsername(v)
	}
	return 0
}

// FeedOnMessage — обработчик новых сообщений для службы: дописать id в ленту
// разрешённого чата сразу, как Telegram прислал сообщение.
func FeedOnMessage(loader func() (*config.Settings, error)) tgc.OnMessage {
	return func(ctx context.Context, c *tgc.Conn, peer tg.PeerClass, msgID int, out bool) {
		s, err := loader()
		if err != nil {
			return
		}
		marked := tgc.MarkedPeerID(peer)
		for _, rule := range s.Rules() {
			if !rule.Read || ruleMarkedID(rule, c) != marked {
				continue
			}
			if out {
				markOutgoing(rule.Alias, msgID)
			}
			if _, err := appendNewer(s, rule.Alias, []int{msgID}); err != nil {
				_ = audit.Log(s.AuditPath(), "feed_write_failed", "chat", rule.Alias, "error", err.Error())
			}
		}
	}
}

// Watch печатает «alias id» на каждое новое сообщение — для Monitor в Claude
// Code (там, где tg_subscribe недоступен). Читает только ленты в базе, к
// Telegram не ходит. after > 0 — сразу выдать и то, что пришло после
// этого id (подписку перезапустили, а сообщения за перерыв терять нельзя).
func Watch(ctx context.Context, s *config.Settings, aliases []string, after int, out io.Writer) error {
	var rules []config.ChatRule
	if len(aliases) > 0 {
		for _, a := range aliases {
			r, err := s.Resolve(a)
			if err != nil {
				return err
			}
			rules = append(rules, r)
		}
	} else {
		for _, r := range s.Rules() {
			if r.Read {
				rules = append(rules, r)
			}
		}
	}
	seen := map[string]int{}
	var names []string
	for _, r := range rules {
		seen[r.Alias] = FeedLast(s, r.Alias)
		if after > 0 {
			seen[r.Alias] = min(seen[r.Alias], after)
		}
		names = append(names, r.Alias)
	}
	fmt.Fprintf(out, "слежу за: %s (Ctrl+C — выход)\n", strings.Join(names, ", "))
	for ctx.Err() == nil {
		for _, alias := range names {
			for _, id := range idsAfter(s, alias, seen[alias]) {
				if _, err := fmt.Fprintf(out, "%s %d\n", alias, id); err != nil {
					// читателя нет (сессия агента закрылась) — не висеть сиротой
					return nil
				}
				seen[alias] = max(seen[alias], id)
			}
		}
		sleepCtx(ctx, 250*time.Millisecond)
	}
	return nil
}
