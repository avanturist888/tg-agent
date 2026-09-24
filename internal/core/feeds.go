package core

// Ленты новых сообщений: data/feeds/<alias>.ids — по id на строку.
//
// Пишет служба: Telegram сам присылает ей новые сообщения (FeedOnMessage), а
// раз в минуту она сверяет верхние сообщения чатов (PollOnce) — на случай
// пропусков и собственных отправок. Агенты файл только читают — это не стоит
// ни сессии, ни сети, ни лока.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"tgagent/internal/audit"
	"tgagent/internal/config"
	"tgagent/internal/tgc"
)

func FeedPath(s *config.Settings, alias string) string {
	return filepath.Join(s.FeedsDir(), alias+".ids")
}

// LastID — последний id в ленте (0, если ленты ещё нет).
func LastID(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0
	}
	start := max(0, st.Size()-64)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return 0
	}
	tail, _ := io.ReadAll(f)
	fields := strings.Fields(string(tail))
	if len(fields) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(fields[len(fields)-1])
	return n
}

func appendIDs(path string, ids []int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(strconv.Itoa(id))
		b.WriteByte('\n')
	}
	_, err = f.WriteString(b.String())
	return err
}

// PollOnce — сверка: дописать во все ленты то, что пришло после последнего
// id. Возвращает, сколько добавлено.
func PollOnce(ctx context.Context, s *config.Settings) (map[string]int, error) {
	added := map[string]int{}
	var rules []config.ChatRule
	for _, r := range s.Rules() {
		if r.Read {
			rules = append(rules, r)
		}
	}
	if len(rules) == 0 {
		return added, nil
	}
	err := tgc.Run(ctx, s, tgc.Opts{Wait: 30 * time.Second, Caller: "poll_once"},
		func(ctx context.Context, c *tgc.Conn) error {
			targets := map[string]tgc.Target{}
			for _, r := range rules {
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
			for alias, topID := range top {
				path := FeedPath(s, alias)
				known := LastID(path)
				if known == 0 {
					// новая лента начинается с текущего момента — историю не льём
					if _, err := appendNewer(path, []int{topID}); err != nil {
						return err
					}
					continue
				}
				if topID <= known {
					continue
				}
				batch, err := c.History(ctx, targets[alias], 200, 0, known, "")
				if err != nil {
					return err
				}
				var ids []int
				for i := len(batch.Messages) - 1; i >= 0; i-- {
					if id := batch.Messages[i].GetID(); id > known {
						ids = append(ids, id)
					}
				}
				n, err := appendNewer(path, ids)
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

// feedMu — ленты дописывают и события, и сверка: по очереди.
var feedMu sync.Mutex

// appendNewer дописывает id, если он новее последнего в ленте. Новая лента
// начинается с текущего сообщения — историю не льём.
func appendNewer(path string, ids []int) (int, error) {
	feedMu.Lock()
	defer feedMu.Unlock()
	last := LastID(path)
	var fresh []int
	for _, id := range ids {
		if id > last {
			fresh = append(fresh, id)
			last = id
		}
	}
	if len(fresh) == 0 {
		return 0, nil
	}
	return len(fresh), appendIDs(path, fresh)
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
	return func(ctx context.Context, c *tgc.Conn, peer tg.PeerClass, msgID int) {
		s, err := loader()
		if err != nil {
			return
		}
		marked := tgc.MarkedPeerID(peer)
		for _, rule := range s.Rules() {
			if !rule.Read || ruleMarkedID(rule, c) != marked {
				continue
			}
			if _, err := appendNewer(FeedPath(s, rule.Alias), []int{msgID}); err != nil {
				_ = audit.Log(s.AuditPath(), "feed_write_failed", "chat", rule.Alias, "error", err.Error())
			}
		}
	}
}

// Watch печатает «alias id» на каждое новое сообщение — для Monitor в Claude
// Code. Читает только локальные файлы лент, к Telegram не ходит.
func Watch(ctx context.Context, s *config.Settings, aliases []string, out io.Writer) error {
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
		seen[r.Alias] = LastID(FeedPath(s, r.Alias))
		names = append(names, r.Alias)
	}
	fmt.Fprintf(out, "слежу за: %s (Ctrl+C — выход)\n", strings.Join(names, ", "))
	for ctx.Err() == nil {
		for _, alias := range names {
			raw, err := os.ReadFile(FeedPath(s, alias))
			if err != nil {
				continue
			}
			best := seen[alias]
			for _, f := range strings.Fields(string(raw)) {
				id, err := strconv.Atoi(f)
				if err != nil || id <= seen[alias] {
					continue
				}
				fmt.Fprintf(out, "%s %d\n", alias, id)
				best = max(best, id)
			}
			seen[alias] = best
		}
		sleepCtx(ctx, time.Second)
	}
	return nil
}
