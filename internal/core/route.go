package core

// Несколько агентов на одном чате. Подписка — это фильтр (от кого, какие
// слова в тексте, тема форума); единоличным остаётся не чтение чата, а право
// ответить на конкретное сообщение — иначе на него ответят двое.
//
// Новое входящее сообщение чата, куда можно писать, получает маршрут: цепочку
// подходящих подписок. Порядок — сначала узкие (от кого / слова), потом тема,
// потом весь чат; при равных — кто подписался раньше (first — вперёд, по
// слову владельца). Будят только первого — он владелец сообщения. Не его —
// tg_pass передаёт следующему с запиской. Молчит, но жив — сообщение остаётся
// за ним (решение владельца 03.11.2026). Сессию не разбудить (Dead) или она
// отписалась, так и не узнав о сообщении, — служба передаёт дальше сама.
//
// В чате только для чтения отвечать некому: там каждого подписчика будят обо
// всём, что подходит под его фильтр, маршрутов нет.
//
// Маршруты — таблица routes в data/state.db (чат, id → цепочка и владелец),
// без текста.
// Текст и автора для фильтров служба читает по id и держит только в памяти.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"tgagent/internal/audit"
	"tgagent/internal/config"
	"tgagent/internal/omap"
)

// subFilter — чем подписка на чат уже, чем «всё подряд».
type subFilter struct {
	// From — от кого: id (строкой) или username без @, в нижнем регистре
	From []string `json:"from,omitempty"`
	// Text — слова или фразы: подходит, если в тексте есть хоть одно
	Text []string `json:"text,omitempty"`
	// First — вперёд среди подписок того же охвата (только по слову владельца)
	First bool `json:"first,omitempty"`
	// Joined — когда подписались на этот чат: при равном охвате раньше — первее
	Joined time.Time `json:"joined"`
}

func (f *subFilter) narrow() bool { return f != nil && (len(f.From) > 0 || len(f.Text) > 0) }

// normFrom — «@Name», «name», «123» → «name» / «123».
func normFrom(v string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(v), "@"))
}

// normText — для сравнения слов: нижний регистр, ё = е.
func normText(v string) string {
	return strings.ReplaceAll(strings.ToLower(v), "ё", "е")
}

// msgMeta — то, по чему фильтруют: автор и текст.
type msgMeta struct {
	FromID   string
	Username string
	Text     string
}

// scope — охват подписки на чат: 0 — по автору или словам, 1 — тема, 2 — весь чат.
func scope(sub *subscription, alias string) int {
	switch {
	case sub.Filters[alias].narrow():
		return 0
	case sub.Topics[alias] != 0:
		return 1
	}
	return 2
}

func joined(sub *subscription, alias string) time.Time {
	if f := sub.Filters[alias]; f != nil && !f.Joined.IsZero() {
		return f.Joined
	}
	return sub.Since
}

// queueOrder — порядок подписок на чат: охват, first, время подписки.
func queueOrder(subs map[string]*subscription, alias string, keys []string) {
	slices.SortStableFunc(keys, func(a, b string) int {
		sa, sb := subs[a], subs[b]
		if d := scope(sa, alias) - scope(sb, alias); d != 0 {
			return d
		}
		fa, fb := sa.Filters[alias] != nil && sa.Filters[alias].First, sb.Filters[alias] != nil && sb.Filters[alias].First
		if fa != fb {
			if fa {
				return -1
			}
			return 1
		}
		if c := joined(sa, alias).Compare(joined(sb, alias)); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
}

// fits — сообщение подходит под фильтр подписки. Неизвестное (не вышло
// прочитать) подходит: лучше лишнее уведомление, чем потерянное сообщение.
func fits(sub *subscription, alias string, m *msgMeta, topic *topicRef) bool {
	if want := sub.Topics[alias]; want != 0 && topic != nil && topic.ID != want {
		return false
	}
	f := sub.Filters[alias]
	if !f.narrow() || m == nil {
		return true
	}
	if len(f.From) > 0 && !slices.Contains(f.From, m.FromID) && (m.Username == "" || !slices.Contains(f.From, m.Username)) {
		return false
	}
	if len(f.Text) > 0 {
		text := normText(m.Text)
		if !slices.ContainsFunc(f.Text, func(w string) bool { return strings.Contains(text, w) }) {
			return false
		}
	}
	return true
}

// ── маршруты ─────────────────────────────────────────────────────────────

type passNote struct {
	From string    `json:"from"` // кто передал (имя для агента)
	Note string    `json:"note,omitempty"`
	At   time.Time `json:"at"`
}

// route — чьё сообщение. Owner = Chain[Pos]; Pos == len(Chain) — ничьё
// (все передали). Told — владельца уже разбудили об этом сообщении.
type route struct {
	Chain []string   `json:"chain"`
	Pos   int        `json:"pos"`
	Told  bool       `json:"told,omitempty"`
	Notes []passNote `json:"notes,omitempty"`
	At    time.Time  `json:"at"`
}

func (r *route) owner() string {
	if r == nil || r.Pos >= len(r.Chain) {
		return ""
	}
	return r.Chain[r.Pos]
}

type routes map[string]map[int]*route

const (
	routesKeep = 500                // маршрутов на чат
	routesTTL  = 7 * 24 * time.Hour // и не старше
)

func scanRoutes(rows *sql.Rows) (routes, map[routeKey]string, error) {
	defer rows.Close()
	rs, raw := routes{}, map[routeKey]string{}
	for rows.Next() {
		var (
			k    routeKey
			body string
		)
		if err := rows.Scan(&k.chat, &k.id, &body); err != nil {
			return nil, nil, err
		}
		r := &route{}
		if json.Unmarshal([]byte(body), r) != nil {
			continue
		}
		if rs[k.chat] == nil {
			rs[k.chat] = map[int]*route{}
		}
		rs[k.chat][k.id] = r
		raw[k] = body
	}
	return rs, raw, rows.Err()
}

type routeKey struct {
	chat string
	id   int
}

// loadRoutes — все маршруты (не прочитались — пусто).
func loadRoutes(s *config.Settings) routes {
	db, err := stateDB(s)
	if err != nil {
		return routes{}
	}
	rows, err := db.Q().QueryContext(context.Background(), "SELECT chat, msg_id, body FROM routes")
	if err != nil {
		return routes{}
	}
	rs, _, err := scanRoutes(rows)
	if err != nil {
		return routes{}
	}
	return rs
}

// loadChatRoutes — маршруты одного чата.
func loadChatRoutes(s *config.Settings, alias string) map[int]*route {
	db, err := stateDB(s)
	if err != nil {
		return nil
	}
	rows, err := db.Q().QueryContext(context.Background(), "SELECT chat, msg_id, body FROM routes WHERE chat = ?", alias)
	if err != nil {
		return nil
	}
	rs, _, err := scanRoutes(rows)
	if err != nil {
		return nil
	}
	return rs[alias]
}

// editRoutes — прочитать, поменять и записать маршруты одной транзакцией;
// пишутся только изменившиеся и убранные.
func editRoutes(s *config.Settings, fn func(rs routes) bool) error {
	db, err := stateDB(s)
	if err != nil {
		return err
	}
	ctx := context.Background()
	return db.Write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT chat, msg_id, body FROM routes")
		if err != nil {
			return err
		}
		rs, raw, err := scanRoutes(rows)
		if err != nil {
			return err
		}
		if !fn(rs) {
			return nil
		}
		prune(rs)
		for chat, m := range rs {
			for id, r := range m {
				body, err := json.Marshal(r)
				if err != nil {
					return err
				}
				k := routeKey{chat, id}
				if raw[k] == string(body) {
					continue
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO routes(chat, msg_id, at, body) VALUES(?, ?, ?, ?)
					ON CONFLICT(chat, msg_id) DO UPDATE SET at = excluded.at, body = excluded.body`,
					chat, id, r.At.Unix(), string(body)); err != nil {
					return err
				}
			}
		}
		for k := range raw {
			if rs[k.chat][k.id] == nil {
				if _, err := tx.ExecContext(ctx, "DELETE FROM routes WHERE chat = ? AND msg_id = ?", k.chat, k.id); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// prune — старые маршруты не нужны: решать по ним уже нечего.
func prune(rs routes) {
	for alias, m := range rs {
		ids := make([]int, 0, len(m))
		for id, r := range m {
			if time.Since(r.At) > routesTTL {
				delete(m, id)
				continue
			}
			ids = append(ids, id)
		}
		if len(ids) > routesKeep {
			slices.Sort(ids)
			for _, id := range ids[:len(ids)-routesKeep] {
				delete(m, id)
			}
		}
		if len(m) == 0 {
			delete(rs, alias)
		}
	}
}

// renameRoutes — сессии одного треда слились (joinThreadSubs): их сообщения
// теперь у into.
func renameRoutes(s *config.Settings, from []string, into string) {
	_ = editRoutes(s, func(rs routes) bool {
		changed := false
		for _, m := range rs {
			for _, r := range m {
				for i, k := range r.Chain {
					if slices.Contains(from, k) {
						r.Chain[i], changed = into, true
					}
				}
				r.Chain = slices.Compact(r.Chain)
				if r.Pos > len(r.Chain) {
					r.Pos = len(r.Chain)
				}
			}
		}
		return changed
	})
}

// metaOf — автор и текст сообщений (для фильтров). Не вышло — nil.
var metaOf = func(ctx context.Context, s *config.Settings, rule config.ChatRule, ids []int) map[int]*msgMeta {
	out := map[int]*msgMeta{}
	var missing []int
	for _, id := range ids {
		if m, ok := memoMeta(rule.Alias, id); ok {
			out[id] = m
		} else {
			missing = append(missing, id)
		}
	}
	for len(missing) > 0 {
		part := missing[:min(len(missing), 50)]
		missing = missing[len(part):]
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		var (
			msgs  []*omap.Map
			err   error
			forum bool
			topic *omap.Map
		)
		o := ReadOpts{IDs: part}
		if n := netOf(s); n != nil {
			msgs, err = n.readChat(cctx, s, rule, o, len(part))
		} else {
			err = readTelegram(cctx, s, rule, o, len(part), &msgs, &forum, &topic)
		}
		cancel()
		if err != nil {
			continue
		}
		for _, m := range msgs {
			id, _ := m.Get("id")
			meta := &msgMeta{}
			if from, ok := m.Get("from"); ok {
				if f, ok := from.(*omap.Map); ok {
					if v, ok := f.Get("id"); ok && v != nil {
						meta.FromID = fmt.Sprint(v)
					}
					if v, ok := f.Get("username"); ok && v != nil {
						meta.Username = normFrom(fmt.Sprint(v))
					}
				}
			}
			if v, ok := m.Get("text"); ok && v != nil {
				meta.Text = fmt.Sprint(v)
			}
			n := int(toInt64(id))
			out[n] = meta
			rememberMeta(rule.Alias, n, meta)
		}
	}
	return out
}

var metaMemo = struct {
	sync.Mutex
	m map[string]map[int]*msgMeta
}{m: map[string]map[int]*msgMeta{}}

func memoMeta(alias string, id int) (*msgMeta, bool) {
	metaMemo.Lock()
	defer metaMemo.Unlock()
	m, ok := metaMemo.m[alias][id]
	return m, ok
}

func rememberMeta(alias string, id int, m *msgMeta) {
	metaMemo.Lock()
	defer metaMemo.Unlock()
	cur := metaMemo.m[alias]
	if cur == nil || len(cur) > 2000 {
		cur = map[int]*msgMeta{}
		metaMemo.m[alias] = cur
	}
	cur[id] = m
}

// routeChats — назначить владельцев новым входящим сообщениям чатов, куда
// можно писать, и передать дальше то, о чём владелец так и не узнал, потому
// что его сессию не разбудить или он отписался. Возвращает маршруты.
func routeChats(ctx context.Context, s *config.Settings, subs map[string]*subscription) routes {
	byChat := map[string][]string{} // alias → подписки
	for key, sub := range subs {
		for alias := range sub.Chats {
			byChat[alias] = append(byChat[alias], key)
		}
	}
	rs := loadRoutes(s)
	type decision struct {
		alias string
		id    int
		r     *route
	}
	var fresh []decision
	var moved []decision
	for alias, keys := range byChat {
		rule, ok := s.Chats[alias]
		if !ok || !rule.Send || s.SendPolicy == "disabled" {
			continue // писать нельзя — отвечать некому, маршрутов нет
		}
		queueOrder(subs, alias, keys)
		from := -1
		for _, k := range keys {
			if v := subs[k].Chats[alias]; from < 0 || v < from {
				from = v
			}
		}
		var ids []int
		for _, id := range idsAfter(s, alias, from) {
			if !isOutgoing(alias, id) && rs[alias][id] == nil {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			var meta map[int]*msgMeta
			if slices.ContainsFunc(keys, func(k string) bool { return subs[k].Filters[alias].narrow() }) {
				meta = metaOf(ctx, s, rule, ids)
			}
			var topics map[int]topicRef
			if slices.ContainsFunc(keys, func(k string) bool { return subs[k].Topics[alias] != 0 }) {
				topics = topicsOf(ctx, s, rule, ids)
			}
			for _, id := range ids {
				var chain []string
				for _, k := range keys {
					sub := subs[k]
					if sub.Chats[alias] >= id {
						continue // подписалась позже — о прошлом не знает
					}
					var tp *topicRef
					if t, ok := topics[id]; ok {
						tp = &t
					}
					if fits(sub, alias, meta[id], tp) {
						chain = append(chain, k)
					}
				}
				if len(chain) == 0 {
					continue // никому не подходит — ничьё, как без подписок
				}
				r := &route{Chain: chain, At: time.Now()}
				r.Pos = nextAlive(subs, alias, chain, -1)
				fresh = append(fresh, decision{alias, id, r})
			}
		}
		// владелец так и не узнал, а будить его некому — дальше по цепочке
		for id, r := range rs[alias] {
			own := r.owner()
			if own == "" || r.Told {
				continue
			}
			sub := subs[own]
			still := false
			if sub != nil {
				_, still = sub.Chats[alias]
			}
			if still && sub.Dead.IsZero() {
				continue
			}
			next := nextAlive(subs, alias, r.Chain, r.Pos)
			if next >= len(r.Chain) {
				continue // дальше живых нет — ждём, вдруг оживёт (или подпишется снова)
			}
			why := "сессия не отвечает"
			if sub == nil || !still {
				why = "отписалась"
			}
			moved = append(moved, decision{alias, id, &route{Pos: next, Notes: []passNote{{From: own, Note: why, At: time.Now()}}}})
		}
	}
	if len(fresh) == 0 && len(moved) == 0 {
		return rs
	}
	_ = editRoutes(s, func(cur routes) bool {
		for _, d := range fresh {
			if cur[d.alias] == nil {
				cur[d.alias] = map[int]*route{}
			}
			if cur[d.alias][d.id] == nil { // решение не переигрываем
				cur[d.alias][d.id] = d.r
			}
		}
		for _, d := range moved {
			if r := cur[d.alias][d.id]; r != nil && !r.Told {
				r.Pos = d.r.Pos
				r.Notes = append(r.Notes, d.r.Notes...)
			}
		}
		rs = cur
		return true
	})
	for _, d := range moved {
		_ = audit.Log(s.AuditPath(), "agent_message_passed", "chat", d.alias, "message_id", d.id,
			"from", d.r.Notes[0].From, "reason", d.r.Notes[0].Note, "auto", true)
	}
	return rs
}

// nextAlive — следующий после pos в цепочке, кто ещё подписан на чат и
// отвечает; никого — len(chain). Сессия только перестала отвечать — её
// пропускаем, если дальше есть живые.
func nextAlive(subs map[string]*subscription, alias string, chain []string, pos int) int {
	for i := pos + 1; i < len(chain); i++ {
		sub := subs[chain[i]]
		if sub == nil {
			continue
		}
		if _, ok := sub.Chats[alias]; ok && sub.Dead.IsZero() {
			return i
		}
	}
	if pos < 0 { // все в цепочке молчат — первым остаётся первый: оживёт — узнает
		for i, k := range chain {
			if sub := subs[k]; sub != nil {
				if _, ok := sub.Chats[alias]; ok {
					return i
				}
			}
		}
	}
	return len(chain)
}

// markTold — владельца разбудили: эти сообщения он видел.
func markTold(s *config.Settings, key string, told map[string][]int) {
	if len(told) == 0 {
		return
	}
	_ = editRoutes(s, func(rs routes) bool {
		changed := false
		for alias, ids := range told {
			for _, id := range ids {
				if r := rs[alias][id]; r != nil && r.owner() == key && !r.Told {
					r.Told, changed = true, true
				}
			}
		}
		return changed
	})
}

// ── tg_pass ──────────────────────────────────────────────────────────────

// Pass — tg_pass: сообщение не этой сессии — передать следующей в цепочке.
func Pass(ctx context.Context, s *config.Settings, chat string, messageID int, note string) (*omap.Map, error) {
	if s.Agent != nil {
		return nil, &Bad{Msg: "Агенту из контейнера передача сообщений недоступна."}
	}
	rule, err := readable(s, chat)
	if err != nil {
		return nil, err
	}
	a := agentFrom(ctx)
	if a == nil {
		return nil, &Bad{Msg: noInbox}
	}
	subs := loadSubs(s)
	me := a.Key()
	mine := sessionThread(ctx, a.Session)
	var (
		passedTo string
		fromName = holderNameKey(ctx, subs, me)
		bad      error
		r2       route
	)
	err = editRoutes(s, func(rs routes) bool {
		r := rs[rule.Alias][messageID]
		if r == nil {
			bad = &Bad{Msg: fmt.Sprintf("Сообщение %d в %s никому не назначено подпиской — передавать нечего.",
				messageID, s.Ref(rule.Alias))}
			return false
		}
		own := r.owner()
		if own != me && (mine == "" || own == "" || sessionThread(ctx, subOrEmpty(subs, own)) != mine) {
			who := "никому (все в цепочке его уже передали)"
			if own != "" {
				who = holderNameKey(ctx, subs, own)
			}
			bad = &Bad{Msg: fmt.Sprintf("Сообщение %d сейчас не твоё, а %s — передать его может только владелец.", messageID, who)}
			return false
		}
		r.Pos = nextAlive(subs, rule.Alias, r.Chain, r.Pos)
		r.Told = false
		r.Notes = append(r.Notes, passNote{From: fromName, Note: strings.TrimSpace(note), At: time.Now()})
		if nxt := r.owner(); nxt != "" {
			passedTo = holderNameKey(ctx, subs, nxt)
		}
		r2 = *r
		return true
	})
	if bad != nil {
		return nil, bad
	}
	if err != nil {
		return nil, err
	}
	_ = audit.Log(s.AuditPath(), "agent_message_passed", "chat", rule.Alias, "message_id", messageID,
		"from", me, "to", r2.owner(), "note", note)
	pokeSubs()
	out := omap.New().Set("chat", s.Ref(rule.Alias)).Set("message_id", messageID)
	if passedTo == "" {
		return out.Set("passed_to", nil).Set("hint", "Дальше в цепочке никого: сообщение осталось без "+
			"обработчика, как в чате без подписок. Если оно ждёт решения владельца — скажи ему."), nil
	}
	return out.Set("passed_to", passedTo).Set("hint", "Передано: тот агент получит уведомление с твоей запиской. "+
		"Больше этим сообщением не занимайся."), nil
}

func subOrEmpty(subs map[string]*subscription, key string) string {
	if sub := subs[key]; sub != nil {
		return sub.Agent.Session
	}
	return ""
}

func holderNameKey(ctx context.Context, subs map[string]*subscription, key string) string {
	if sub := subs[key]; sub != nil {
		return holderName(ctx, sub)
	}
	return "сессия " + key
}

// queueView — кто слушает чат и в каком порядке (для ответа tg_subscribe).
func queueView(ctx context.Context, s *config.Settings, subs map[string]*subscription, alias, me string) []*omap.Map {
	var keys []string
	for k, sub := range subs {
		if _, ok := sub.Chats[alias]; ok {
			keys = append(keys, k)
		}
	}
	queueOrder(subs, alias, keys)
	out := make([]*omap.Map, 0, len(keys))
	for _, k := range keys {
		sub := subs[k]
		row := omap.New()
		if k == me {
			row.Set("you", true)
		} else {
			row.Set("session", holderName(ctx, sub))
		}
		row.Set("filter", filterLabel(sub, alias))
		out = append(out, row)
	}
	return out
}

// filterLabel — охват подписки словами.
func filterLabel(sub *subscription, alias string) string {
	var parts []string
	if f := sub.Filters[alias]; f.narrow() {
		if len(f.From) > 0 {
			parts = append(parts, "от "+strings.Join(f.From, ", "))
		}
		if len(f.Text) > 0 {
			q := make([]string, len(f.Text))
			for i, w := range f.Text {
				q[i] = strconv.Quote(w)
			}
			parts = append(parts, "со словами "+strings.Join(q, " / "))
		}
	}
	if t := sub.Topics[alias]; t != 0 {
		parts = append(parts, fmt.Sprintf("тема %d", t))
	}
	if len(parts) == 0 {
		return "весь чат"
	}
	return strings.Join(parts, "; ")
}
