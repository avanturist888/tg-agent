package core

// Ленты новых сообщений: data/feeds/<alias>.ids — по id на строку.
//
// Пишет только слушатель (демон): раз в несколько секунд одним запросом
// смотрит верхние сообщения всех разрешённых чатов и дописывает новые id.
// Агенты файл только читают — это не стоит ни сессии, ни сети, ни лока.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tgagent/internal/audit"
	"tgagent/internal/config"
	"tgagent/internal/lock"
	"tgagent/internal/tgc"
)

// PollLimit — секунд на один проход; дольше — бросаем и ждём следующего.
const PollLimit = 30 * time.Second

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

// PollOnce — один проход: дописать новые id во все ленты. Возвращает, сколько добавлено.
func PollOnce(ctx context.Context, s *config.Settings, background bool) (map[string]int, error) {
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
	// короткое ожидание: сессия занята — пропускаем проход, а не стоим
	err := tgc.Run(ctx, s, tgc.Opts{Wait: 500 * time.Millisecond, Background: background, Caller: "poll_once"},
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
					if err := appendIDs(path, []int{topID}); err != nil {
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
				if len(ids) > 0 {
					if err := appendIDs(path, ids); err != nil {
						return err
					}
					added[alias] = len(ids)
				}
			}
			return nil
		})
	return added, err
}

// RunPoller — фоновый цикл внутри демона. Сбой одного прохода не роняет демона;
// сессию проход отдаёт отправке по первому требованию.
func RunPoller(ctx context.Context, loader func() (*config.Settings, error), interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	for ctx.Err() == nil {
		s, err := loader() // белый список мог поменяться
		if err == nil {
			pctx, cancel := context.WithTimeout(ctx, PollLimit)
			_, err = PollOnce(pctx, s, true)
			timedOut := errors.Is(pctx.Err(), context.DeadlineExceeded)
			cancel()
			switch {
			case err == nil, lock.IsBusy(err):
				// сессию держит отправка или чтение — зайдём на следующем круге
			case timedOut:
				_ = audit.Log(s.AuditPath(), "feed_poll_timeout", "seconds", int(PollLimit.Seconds()))
			case ctx.Err() != nil:
				return
			case errors.Is(err, context.Canceled):
				// уступили сессию отправке
			default:
				_ = audit.Log(s.AuditPath(), "feed_poll_failed", "error", fmt.Sprintf("%v", err))
			}
		}
		sleepCtx(ctx, interval)
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

var _ = audit.Now
