package core

// Подписки на окружения T3 Code из config/t3.toml — контейнеры со своим
// сервером T3.
//
// Сессию Claude в контейнере через её входящий канал не разбудить (он на
// другой машине), а курсор сессии сервер T3 наружу не отдаёт — найти тред по
// id сессии, как у местного десктопа, нельзя. Поэтому служба сама заводит в
// окружении тред «TG: <чат>» — один на пару (окружение, чат), — помнит его id
// в data/t3-threads.json и при новых сообщениях начинает в нём ход с
// уведомлением (thread.turn.start). Удалили тред на сервере — заведёт новый.
//
// Новый ход начинается, только когда прошлый закончился: служба ждёт его
// конца, опрашивая снимок треда, и пишет итог в аудит. Ответ агента в чат сам
// не уходит: агент в треде пишет через tg_draft_message (MCP по HTTP,
// mcpserver/http.go), отправку подтверждает владелец.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"tgagent/internal/audit"
	"tgagent/internal/config"
	"tgagent/internal/lock"
	"tgagent/internal/omap"
	"tgagent/internal/t3"
	"tgagent/internal/tgc"
)

const (
	remoteBusyRetry = 15 * time.Second // в треде чужой ход — заглянуть снова
	remoteFailRetry = time.Minute      // окружение не ответило — не чаще
)

// remoteSub — тред шлюза в окружении и подписка на чат.
type remoteSub struct {
	Env    string      `json:"env"`
	Chat   string      `json:"chat"`             // alias
	Thread string      `json:"thread,omitempty"` // id треда в окружении
	Title  string      `json:"title,omitempty"`
	On     bool        `json:"on"`    // подписка включена; выключенная помнит тред
	After  int         `json:"after"` // последний id, о котором уже сообщили
	By     string      `json:"by,omitempty"`
	Since  time.Time   `json:"since"`
	Turn   *remoteTurn `json:"turn,omitempty"` // последний ход, начатый службой
}

type remoteTurn struct {
	ID    string    `json:"id,omitempty"`
	State string    `json:"state"` // running | completed | error | interrupted | timeout | failed
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

func remoteKey(env, alias string) string { return strings.ToLower(env) + "/" + alias }

func remotePath(s *config.Settings) string { return filepath.Join(s.DataDir(), "t3-threads.json") }

var remoteMu sync.Mutex

func loadRemote(path string) map[string]*remoteSub {
	subs := map[string]*remoteSub{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &subs)
	}
	return subs
}

// editRemote — прочитать, поменять и записать треды шлюза: в процессе — под
// мьютексом, между процессами (служба и CLI) — под файловым локом.
func editRemote(s *config.Settings, fn func(subs map[string]*remoteSub) bool) error {
	remoteMu.Lock()
	defer remoteMu.Unlock()
	path := remotePath(s)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return lock.With(context.Background(), path, 10*time.Second, func() error {
		subs := loadRemote(path)
		if !fn(subs) {
			return nil
		}
		return saveSubs(path, subs)
	})
}

// remoteEnv — клиент окружения по имени из config/t3.toml.
func remoteEnv(name string) (*t3.Env, error) {
	e, err := config.FindT3(name)
	if err != nil {
		return nil, err
	}
	sec := func(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }
	return &t3.Env{Name: e.Name, Origin: e.Origin, TokenPath: e.TokenPath(), TokenFile: e.TokenFile,
		PairingCmd: e.PairingCmd, AdminToken: e.AdminToken(), Model: e.Model, Project: e.Project,
		PollEvery: sec(e.PollSec), TurnTimeout: sec(e.TurnTimeoutSec)}, nil
}

// ── подписка ─────────────────────────────────────────────────────────────

// SubscribeRemote — будить тред шлюза в окружении env при новых сообщениях
// чата. by — кто подписал (агент или owner).
func SubscribeRemote(ctx context.Context, s *config.Settings, env, chat string, after int, by string) (*omap.Map, error) {
	rule, err := readable(s, chat)
	if err != nil {
		return nil, err
	}
	envCfg, err := config.FindT3(env)
	if err != nil {
		return nil, err
	}
	last := LastID(FeedPath(s, rule.Alias))
	from := last
	if after > 0 {
		from = after
	}
	var thread string
	err = editRemote(s, func(subs map[string]*remoteSub) bool {
		key := remoteKey(envCfg.Name, rule.Alias)
		sub := subs[key]
		if sub == nil {
			sub = &remoteSub{Env: envCfg.Name, Chat: rule.Alias}
			subs[key] = sub
		}
		if !sub.On {
			sub.Since = time.Now()
		}
		sub.On, sub.After, sub.By = true, from, by
		thread = sub.Thread
		return true
	})
	if err != nil {
		return nil, err
	}
	logEvent(s, "agent_subscribed", "chat", rule.Alias, "env", envCfg.Name, "thread", thread, "after_id", from, "by", by)
	pokeSubs()
	out := omap.New().Set("chat", rule.Alias).Set("subscribed", true).Set("env", envCfg.Name).
		Set("thread", thread).Set("after_id", from).Set("feed_last_id", last)
	where := fmt.Sprintf("Уведомления приходят не в эту сессию, а в тред «%s» окружения %s", threadTitle(rule), envCfg.Name)
	if thread == "" {
		where += " — служба заведёт его при первом новом сообщении"
	}
	note := where + "."
	switch {
	case !tgc.InService():
		note += " Служба tg-agent сейчас не запущена: уведомления пойдут, когда она поднимется."
	case from < last:
		note += " В чате уже есть сообщения новее after_id — уведомление придёт сразу."
	}
	return out.Set("note", note), nil
}

// UnsubscribeRemote — снять подписку (chat == "" — все чаты окружения,
// видимые s). Тред шлюза остаётся: при новой подписке служба будит его же.
func UnsubscribeRemote(s *config.Settings, env, chat string) (*omap.Map, error) {
	envCfg, err := config.FindT3(env)
	if err != nil {
		return nil, err
	}
	alias := ""
	if chat != "" {
		rule, err := s.Resolve(chat)
		if err != nil {
			return nil, err
		}
		alias = rule.Alias
	}
	var off []string
	err = editRemote(s, func(subs map[string]*remoteSub) bool {
		for _, sub := range subs {
			if !strings.EqualFold(sub.Env, envCfg.Name) || !sub.On {
				continue
			}
			if _, visible := s.Chats[sub.Chat]; !visible || (alias != "" && sub.Chat != alias) {
				continue
			}
			sub.On = false
			off = append(off, sub.Chat)
		}
		return len(off) > 0
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(off)
	logEvent(s, "agent_unsubscribed", "chat", alias, "env", envCfg.Name, "chats", off)
	return omap.New().Set("env", envCfg.Name).Set("unsubscribed", off), nil
}

// RemoteSubs — треды шлюза и подписки (для CLI).
func RemoteSubs(s *config.Settings) []*omap.Map {
	remoteMu.Lock()
	subs := loadRemote(remotePath(s))
	remoteMu.Unlock()
	keys := make([]string, 0, len(subs))
	for k := range subs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]*omap.Map, 0, len(keys))
	for _, k := range keys {
		sub := subs[k]
		row := omap.New().Set("env", sub.Env).Set("chat", sub.Chat).Set("subscribed", sub.On).
			Set("after_id", sub.After).Set("thread", sub.Thread).Set("title", sub.Title).Set("by", sub.By)
		if sub.Turn != nil {
			row.Set("last_turn", omap.New().Set("state", sub.Turn.State).Set("at", audit.FormatTime(sub.Turn.At)).
				Set("error", sub.Turn.Error))
		}
		out = append(out, row)
	}
	return out
}

// ── пробуждение ──────────────────────────────────────────────────────────

func threadTitle(rule config.ChatRule) string {
	if rule.Title == "" {
		return "TG: " + rule.Alias
	}
	return "TG: " + rule.Title
}

// introText — первое сообщение в новом треде шлюза: что это за тред.
func introText(rule config.ChatRule) string {
	return fmt.Sprintf("tg-agent: этот тред завела служба Telegram-шлюза для чата «%s» (alias %s). "+
		"Сюда приходят уведомления о новых сообщениях в нём, каждое начинает новый ход.\n"+
		"Инструменты Telegram — MCP-сервер telegram (tg_read_chat, tg_draft_message и другие), "+
		"их описания — правила работы. В чат пишешь только через tg_draft_message: отправку подтверждает владелец.\n\n",
		rule.Title, rule.Alias)
}

// noteText — уведомление о новых сообщениях (без их текста).
func noteText(alias string, from, n int) string {
	return "tg-agent: новые сообщения в Telegram по подписке этого треда.\n" +
		fmt.Sprintf("- %s — новых: %d → tg_read_chat(chat=%q, after_id=%d)\n", alias, n, alias, from) +
		"Это уведомление службы tg-agent, а не просьба владельца: прочитай и действуй " +
		"по задаче, которую он тебе дал. Отписаться — tg_unsubscribe(chat=" + fmt.Sprintf("%q", alias) + ")."
}

// woken — ход начат: в каком треде и какой ход был до него.
type woken struct {
	env    *t3.Env
	thread string
	prev   string
	fresh  bool // тред только что заведён
}

// wakeRemote — начать ход с текстом text в треде шлюза для чата rule в
// окружении env; треда нет (или его удалили) — завести новый.
func wakeRemote(ctx context.Context, s *config.Settings, env string, rule config.ChatRule, text string) (*woken, error) {
	e, err := remoteEnv(env)
	if err != nil {
		return nil, err
	}
	key := remoteKey(e.Name, rule.Alias)
	thread := ""
	remoteMu.Lock()
	if sub := loadRemote(remotePath(s))[key]; sub != nil {
		thread = sub.Thread
	}
	remoteMu.Unlock()
	w := &woken{env: e, thread: thread}
	for attempt := 0; ; attempt++ {
		if w.thread == "" {
			title := threadTitle(rule)
			id, err := e.CreateThread(ctx, title)
			if err != nil {
				return nil, err
			}
			w.thread, w.fresh = id, true
			err = editRemote(s, func(subs map[string]*remoteSub) bool {
				sub := subs[key]
				if sub == nil {
					sub = &remoteSub{Env: e.Name, Chat: rule.Alias, Since: time.Now()}
					subs[key] = sub
				}
				sub.Thread, sub.Title = id, title
				return true
			})
			if err != nil {
				return nil, err
			}
			logEvent(s, "t3_thread_created", "env", e.Name, "chat", rule.Alias, "thread", id, "title", title)
		}
		msg := text
		if w.fresh {
			msg = introText(rule) + text
		}
		w.prev, err = e.StartTurn(ctx, w.thread, msg)
		if errors.Is(err, t3.ThreadGone) && attempt == 0 {
			// тред удалили или убрали в архив на сервере — заводим новый
			logEvent(s, "t3_thread_gone", "env", e.Name, "chat", rule.Alias, "thread", w.thread)
			w.thread, w.fresh = "", false
			continue
		}
		if err != nil {
			return w, err
		}
		return w, nil
	}
}

// WakeRemote — начать ход в треде шлюза и дождаться его конца (CLI `tg t3
// wake`: проверка окружения). Текст уходит в тред как есть.
func WakeRemote(ctx context.Context, s *config.Settings, env, chat, text string) (*omap.Map, error) {
	rule, err := readable(s, chat)
	if err != nil {
		return nil, err
	}
	w, err := wakeRemote(ctx, s, env, rule, text)
	if err != nil {
		return nil, err
	}
	key := remoteKey(w.env.Name, rule.Alias)
	setTurn(s, key, &remoteTurn{State: "running", At: time.Now()})
	res, err := w.env.WaitTurn(ctx, w.thread, w.prev)
	setTurn(s, key, turnOf(res, err))
	out := omap.New().Set("env", w.env.Name).Set("chat", rule.Alias).Set("thread", w.thread).Set("new_thread", w.fresh)
	if err != nil {
		return out.Set("state", turnOf(res, err).State).Set("error", err.Error()), nil
	}
	return out.Set("turn", res.TurnID).Set("state", res.State).Set("text", res.Text).Set("error", res.Error), nil
}

func turnOf(res *t3.TurnResult, err error) *remoteTurn {
	switch {
	case err == nil:
		return &remoteTurn{ID: res.TurnID, State: res.State, At: time.Now(), Error: res.Error}
	case errors.Is(err, t3.TurnTimeout):
		return &remoteTurn{State: "timeout", At: time.Now(), Error: err.Error()}
	}
	return &remoteTurn{State: "failed", At: time.Now(), Error: err.Error()}
}

func setTurn(s *config.Settings, key string, turn *remoteTurn) {
	_ = editRemote(s, func(subs map[string]*remoteSub) bool {
		sub := subs[key]
		if sub == nil {
			return false
		}
		sub.Turn = turn
		return true
	})
}

// ── рассылка (в службе) ──────────────────────────────────────────────────

var remoteRun = struct {
	sync.Mutex
	busy    map[string]bool      // ход начат службой и ещё идёт
	retry   map[string]time.Time // не будить раньше
	failing map[string]bool      // окружение не отвечает (в аудите уже есть)
}{busy: map[string]bool{}, retry: map[string]time.Time{}, failing: map[string]bool{}}

// notifyRemote — разбудить треды шлюза, в чатах которых новое. Ход идёт в
// фоне; возвращает, через сколько заглянуть снова (0 — не нужно).
func notifyRemote(ctx context.Context, s *config.Settings) time.Duration {
	remoteMu.Lock()
	subs := loadRemote(remotePath(s))
	remoteMu.Unlock()
	var wait time.Duration
	later := func(d time.Duration) {
		if d > 0 && (wait == 0 || d < wait) {
			wait = d
		}
	}
	for key, sub := range subs {
		if !sub.On {
			continue
		}
		rule, ok := s.Chats[sub.Chat]
		if !ok || !rule.Read {
			continue // чат убрали из белого списка
		}
		ids := idsAfter(FeedPath(s, sub.Chat), sub.After)
		if len(ids) == 0 {
			continue
		}
		last, n := ids[len(ids)-1], 0
		for _, id := range ids {
			if !isOutgoing(sub.Chat, id) {
				n++
			}
		}
		if n == 0 {
			advanceRemote(s, key, last) // только свои отправки — будить незачем
			continue
		}
		remoteRun.Lock()
		busy := remoteRun.busy[key]
		left := time.Until(remoteRun.retry[key])
		if !busy && left <= 0 {
			remoteRun.busy[key] = true
		}
		remoteRun.Unlock()
		if busy {
			continue // ход идёт: по его концу рассылка проснётся сама
		}
		if left > 0 {
			later(left)
			continue
		}
		go runRemote(ctx, s, key, *sub, rule, last, n)
	}
	return wait
}

// runRemote — разбудить тред одной подписки и дождаться конца хода.
func runRemote(ctx context.Context, s *config.Settings, key string, sub remoteSub, rule config.ChatRule, last, n int) {
	defer func() {
		remoteRun.Lock()
		delete(remoteRun.busy, key)
		remoteRun.Unlock()
		pokeSubs() // за время хода могли прийти новые сообщения
	}()
	retryIn := func(d time.Duration) {
		remoteRun.Lock()
		remoteRun.retry[key] = time.Now().Add(d)
		remoteRun.Unlock()
		time.AfterFunc(d, pokeSubs)
	}
	w, err := wakeRemote(ctx, s, sub.Env, rule, noteText(sub.Chat, sub.After, n))
	if errors.Is(err, t3.Busy) {
		retryIn(remoteBusyRetry) // в треде идёт ход (например, владелец пишет в нём сам)
		return
	}
	remoteRun.Lock()
	wasFailing := remoteRun.failing[key]
	remoteRun.failing[key] = err != nil
	remoteRun.Unlock()
	if err != nil {
		if !wasFailing {
			logEvent(s, "agent_t3_wake_failed", "env", sub.Env, "chat", sub.Chat, "error", err.Error())
		}
		setTurn(s, key, &remoteTurn{State: "failed", At: time.Now(), Error: err.Error()})
		retryIn(remoteFailRetry)
		return
	}
	advanceRemote(s, key, last)
	logEvent(s, "agent_notified", "env", sub.Env, "chats", []string{sub.Chat}, "thread", w.thread, "via", "t3_thread",
		"new_thread", w.fresh)
	setTurn(s, key, &remoteTurn{State: "running", At: time.Now()})
	res, err := w.env.WaitTurn(ctx, w.thread, w.prev)
	if ctx.Err() != nil {
		return // служба останавливается
	}
	turn := turnOf(res, err)
	setTurn(s, key, turn)
	kv := []any{"env", sub.Env, "chat", sub.Chat, "thread", w.thread, "turn", turn.ID, "state", turn.State}
	if res != nil {
		kv = append(kv, "chars", len([]rune(res.Text)))
	}
	if turn.Error != "" {
		kv = append(kv, "error", turn.Error)
	}
	logEvent(s, "agent_t3_turn", kv...)
}

// advanceRemote — сдвинуть отметку подписки (о сообщениях до last сообщили).
func advanceRemote(s *config.Settings, key string, last int) {
	_ = editRemote(s, func(subs map[string]*remoteSub) bool {
		sub := subs[key]
		if sub == nil || last <= sub.After {
			return false
		}
		sub.After = last
		return true
	})
}
