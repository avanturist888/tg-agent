package core

// Аккаунт MAX (профиль с TG_NETWORK=max, internal/maxc). Служба такого
// профиля вместо соединения с Telegram держит клиент MAX: пишет ленты и
// изменения для подписок по его событиям, после каждого входа наверстывает
// пропущенное. Белый список, черновики, карточки и подписки — общие с
// Telegram; здесь — только то, что ходит в саму сеть: чтение, доставка,
// правка, список диалогов.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/maxc"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
)

// MaxSessionPath — сессия MAX профиля (tg --profile max max login).
func MaxSessionPath() string { return filepath.Join(config.DataPath(), "max", "session.json") }

// maxRT — соединение службы и то, что она знает об аккаунте.
var maxRT struct {
	sync.Mutex
	client   *maxc.Client
	self     int64
	names    map[int64]string    // id человека → имя
	chats    map[int64]maxc.Chat // из последнего входа
	ua       maxc.UserAgent      // как служба представилась (для загрузки голосовых)
	contacts map[int64]bool      // контакты владельца (правила папок)
}

// ErrNoMax — без службы MAX сети нет: соединение держит только она.
var ErrNoMax = errors.New("Служба аккаунта MAX не запущена (задача Планировщика профиля max) — без неё MAX недоступен.")

func maxConn(ctx context.Context) (*maxc.Conn, error) {
	maxRT.Lock()
	cl := maxRT.client
	maxRT.Unlock()
	if cl == nil {
		return nil, ErrNoMax
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := cl.Conn(wctx)
	if err != nil {
		return nil, fmt.Errorf("нет связи с MAX: %w", err)
	}
	return c, nil
}

// maxChatID — id чата MAX из белого списка.
func maxChatID(rule config.ChatRule) (int64, error) {
	switch p := rule.Peer.(type) {
	case int64:
		return p, nil
	case string:
		if n, err := strconv.ParseInt(p, 10, 64); err == nil {
			return n, nil
		}
	}
	return 0, &Bad{Msg: fmt.Sprintf("У чата '%s' в белом списке нет числового id чата MAX.", rule.Alias)}
}

// maxRule — чат белого списка по id чата MAX.
func maxRule(s *config.Settings, chatID int64) (config.ChatRule, bool) {
	for _, r := range s.Rules() {
		if id, err := maxChatID(r); err == nil && id == chatID {
			return r, true
		}
	}
	return config.ChatRule{}, false
}

// ── служба ───────────────────────────────────────────────────────────────

// RunMax — соединение с MAX в службе профиля MAX. Сессию завершили на
// стороне MAX — один раз сказать владельцу и ждать новой (tg max login).
func RunMax(ctx context.Context, loader func() (*config.Settings, error)) {
	warned := ""
	for ctx.Err() == nil {
		sess, err := maxc.LoadSession(MaxSessionPath())
		if err != nil {
			if !errors.Is(err, maxc.ErrNoSession) {
				if s, lerr := loader(); lerr == nil {
					logEvent(s, "max_session_failed", "error", err.Error())
				}
			}
			sleepCtx(ctx, time.Minute)
			continue
		}
		if sess.Token == warned { // отозванную не пробуем, ждём новую
			sleepCtx(ctx, time.Minute)
			continue
		}
		cl := &maxc.Client{Session: sess}
		cl.OnLogin = func(r *maxc.LoginResult) { maxLoggedIn(ctx, loader, r) }
		cl.OnEvent = func(ev maxc.Event) { maxOnEvent(loader, ev) }
		cl.OnDisconnect = func(err error) {
			if s, lerr := loader(); lerr == nil {
				logEvent(s, "max_disconnected", "error", err.Error())
			}
		}
		maxRT.Lock()
		maxRT.client, maxRT.ua = cl, sess.UserAgent
		maxRT.Unlock()
		err = cl.Run(ctx)
		maxRT.Lock()
		maxRT.client = nil
		maxRT.Unlock()
		if errors.Is(err, maxc.ErrTokenRevoked) {
			warned = sess.Token
			if s, lerr := loader(); lerr == nil {
				logEvent(s, "max_session_revoked")
				if s.BotReady() {
					_ = sendPinned(ctx, s, "⚠️ <b>Сессия MAX завершена</b>\n\nMAX больше не принимает вход шлюза "+
						"(выход на телефоне или «со всех устройств»). Агентам MAX недоступен.\n\n"+
						"Войти заново: <code>tg --profile "+config.Profile()+" max login --force</code>", "max-session")
				}
			}
		}
	}
}

// maxLoggedIn — после входа: имена, затем ленты читаемых чатов догоняют то,
// что пришло, пока связи не было.
func maxLoggedIn(ctx context.Context, loader func() (*config.Settings, error), r *maxc.LoginResult) {
	maxRT.Lock()
	maxRT.self = r.Profile.Contact.ID
	if maxRT.names == nil {
		maxRT.names = map[int64]string{}
	}
	maxRT.names[r.Profile.Contact.ID] = r.Profile.Contact.DisplayName()
	maxRT.contacts = map[int64]bool{}
	for _, c := range r.Contacts {
		maxRT.names[c.ID] = c.DisplayName()
		maxRT.contacts[c.ID] = true
	}
	maxRT.chats = map[int64]maxc.Chat{}
	for _, ch := range r.Chats {
		maxRT.chats[ch.ID] = ch
	}
	maxRT.Unlock()
	s, err := loader()
	if err != nil {
		return
	}
	logEvent(s, "max_connected", "chats", len(r.Chats))
	go func() { // наверстать — не задерживая события
		c, err := maxConn(ctx)
		if err != nil {
			return
		}
		for _, rule := range s.Rules() {
			id, err := maxChatID(rule)
			if !rule.Read || err != nil {
				continue
			}
			hist, err := c.History(ctx, id, 0, 30)
			if err != nil {
				logEvent(s, "max_catchup_failed", "chat", rule.Alias, "error", err.Error())
				continue
			}
			maxFeed(s, rule, hist)
		}
	}()
}

// maxFeed — новые сообщения чата в его ленту (свои — помечены, о них
// подпискам не говорим).
func maxFeed(s *config.Settings, rule config.ChatRule, msgs []maxc.Message) {
	maxRT.Lock()
	self := maxRT.self
	maxRT.Unlock()
	chatID, _ := maxChatID(rule)
	ids := make([]int, 0, len(msgs))
	for _, m := range msgs {
		if m.Status == "REMOVED" {
			continue
		}
		// в «Избранном» всё от владельца — будит, как в Telegram; своё
		// отправленное шлюзом помечает maxDeliver
		if m.Sender == self && chatID != maxc.SavedChat {
			markOutgoing(rule.Alias, int(m.ID))
		}
		ids = append(ids, int(m.ID))
	}
	slices.Sort(ids)
	if _, err := appendNewer(FeedPath(s, rule.Alias), ids); err != nil {
		logEvent(s, "feed_failed", "chat", rule.Alias, "error", err.Error())
	}
}

func maxOnEvent(loader func() (*config.Settings, error), ev maxc.Event) {
	maxTrack(ev)
	if ev.Kind != maxc.EventNew && ev.Kind != maxc.EventEdit && ev.Kind != maxc.EventDelete {
		return
	}
	s, err := loader()
	if err != nil {
		return
	}
	rule, ok := maxRule(s, ev.ChatID)
	if !ok || !rule.Read {
		return // чат не в белом списке — его событий шлюз не касается
	}
	switch ev.Kind {
	case maxc.EventNew:
		maxFeed(s, rule, []maxc.Message{*ev.Message})
	case maxc.EventEdit:
		noteChange(s, rule.Alias, []int{int(ev.Message.ID)}, false)
	case maxc.EventDelete:
		ids := make([]int, len(ev.MessageIDs))
		for i, id := range ev.MessageIDs {
			ids[i] = int(id)
		}
		noteChange(s, rule.Alias, ids, true)
	}
}

// ── чтение ───────────────────────────────────────────────────────────────

func maxReadChat(ctx context.Context, s *config.Settings, rule config.ChatRule, o ReadOpts, limit int) ([]*omap.Map, error) {
	if o.Topic != 0 {
		return nil, &Bad{Msg: "В MAX тем нет — читай без topic."}
	}
	chatID, err := maxChatID(rule)
	if err != nil {
		return nil, err
	}
	c, err := maxConn(ctx)
	if err != nil {
		return nil, err
	}
	var msgs []maxc.Message
	after, before := int64(o.AfterID), int64(o.BeforeID)
	switch {
	case o.Search != "":
		msgs, err = maxSearch(ctx, c, chatID, o.Search, limit)
	case len(o.IDs) > 0:
		seen := map[int64]bool{}
		for _, id := range o.IDs {
			if _, m := maxFixID(ctx, c, chatID, int64(id)); m != nil && !seen[m.ID] {
				seen[m.ID] = true
				msgs = append(msgs, *m)
			}
		}
	case after > 0:
		after, _ = maxFixID(ctx, c, chatID, after)
		msgs, err = maxAfter(ctx, c, chatID, after, limit)
	case before > 0:
		before, _ = maxFixID(ctx, c, chatID, before)
		msgs, err = c.History(ctx, chatID, maxIDTime(before)+1, limit+2)
	default:
		msgs, err = c.History(ctx, chatID, 0, limit)
	}
	if err != nil {
		return nil, err
	}
	slices.SortFunc(msgs, func(a, b maxc.Message) int { return compareInt(a.ID, b.ID) })
	var kept []maxc.Message
	for _, m := range msgs {
		if m.Status == "REMOVED" || (before > 0 && m.ID >= before) || (after > 0 && m.ID <= after) {
			continue // удалённое агенту не показываем, как и в Telegram
		}
		kept = append(kept, m)
	}
	if len(o.IDs) == 0 && len(kept) > limit {
		if o.AfterID > 0 {
			kept = kept[:limit] // после after_id — ближайшие к нему
		} else {
			kept = kept[len(kept)-limit:]
		}
	}
	out := make([]*omap.Map, len(kept))
	for i, m := range kept {
		out[i] = maxSerialize(ctx, c, m)
	}
	if o.Transcribe {
		maxAttachTranscripts(ctx, s, rule, c, chatID, kept, out)
	}
	return out, nil
}

// Id сообщения MAX — время в мс, сдвинутое на 16 бит, плюс счётчик: около
// 1,2·10¹⁷, больше 2⁵³. Агент (Claude Code, JavaScript) передаёт числа через
// double, и младшие разряды теряются (±8). Время в id при этом цело, поэтому
// точный id находится среди сообщений той же миллисекунды.
const maxIDSlack = 32

func maxIDTime(id int64) int64 { return id >> 16 }

// maxFixID — точный id сообщения чата по, возможно, округлённому; не нашли —
// как есть.
func maxFixID(ctx context.Context, c *maxc.Conn, chatID, id int64) (int64, *maxc.Message) {
	if exact, err := c.Messages(ctx, chatID, []int64{id}); err == nil && len(exact) > 0 && exact[0].ID == id {
		return id, &exact[0]
	}
	near, err := c.HistoryForward(ctx, chatID, maxIDTime(id)-1, 10)
	if err != nil {
		return id, nil
	}
	var best *maxc.Message
	for i := range near {
		d := near[i].ID - id
		if d < 0 {
			d = -d
		}
		if d <= maxIDSlack && (best == nil || d < abs64(best.ID-id)) {
			best = &near[i]
		}
	}
	if best == nil {
		return id, nil
	}
	return best.ID, best
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// maxAfter — сообщения новее id: от его времени вперёд.
func maxAfter(ctx context.Context, c *maxc.Conn, chatID, after int64, limit int) ([]maxc.Message, error) {
	return c.HistoryForward(ctx, chatID, maxIDTime(after), limit+2)
}

func compareInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// maxName — имя человека; незнакомых спрашиваем у сервера (и помним).
func maxName(ctx context.Context, c *maxc.Conn, id int64) string {
	maxRT.Lock()
	name, ok := maxRT.names[id]
	maxRT.Unlock()
	if ok {
		return name
	}
	if contacts, err := c.Contacts(ctx, []int64{id}); err == nil {
		maxRT.Lock()
		if maxRT.names == nil {
			maxRT.names = map[int64]string{}
		}
		for _, ct := range contacts {
			maxRT.names[ct.ID] = ct.DisplayName()
		}
		name = maxRT.names[id]
		maxRT.Unlock()
	}
	return name
}

// maxSerialize — сообщение MAX в том же виде, что и сообщение Telegram.
func maxSerialize(ctx context.Context, c *maxc.Conn, m maxc.Message) *omap.Map {
	out := omap.New().Set("id", m.ID).Set("date", time.UnixMilli(m.Time).UTC().Format(time.RFC3339))
	if m.Sender != 0 {
		out.Set("from", omap.New().Set("id", m.Sender).Set("name", maxName(ctx, c, m.Sender)))
	}
	out.Set("text", m.Text)
	if l := m.Link; l != nil {
		switch l.Type {
		case "REPLY":
			out.Set("reply_to", int64(l.MessageID))
			if l.Message != nil && l.Message.Status != "REMOVED" {
				out.Set("reply_to_message", maxSerialize(ctx, c, *l.Message))
			}
		case "FORWARD":
			fwd := omap.New()
			if l.Message != nil {
				if l.Message.Sender != 0 {
					fwd.Set("from", omap.New().Set("id", l.Message.Sender).Set("name", maxName(ctx, c, l.Message.Sender)))
				}
				fwd.Set("date", time.UnixMilli(l.Message.Time).UTC().Format(time.RFC3339))
				if m.Text == "" {
					out.Set("text", l.Message.Text) // пересланный текст — в самом сообщении-источнике
				}
			}
			out.Set("forwarded_from", fwd)
		}
	}
	for _, a := range m.Attaches {
		if attachType(a) == "CONTROL" { // служебное: вошёл, вышел, сменил название…
			out.Set("service_action", attachStr(a, "event"))
		}
	}
	if a := maxMedia(&m); a != nil {
		out.Set("media", maxMediaKind(a)) // скачать — tg_download_file, картинку — tg_view_media
		maxMediaInfo(out, a)
	}
	if r := m.Reactions; r != nil && len(r.Counters) > 0 {
		list := make([]*omap.Map, 0, len(r.Counters))
		for _, c := range r.Counters {
			list = append(list, omap.New().Set("emoji", c.Reaction).Set("count", c.Count).Set("mine", c.Reaction == r.Mine))
		}
		out.Set("reactions", list)
	}
	if m.Status == "EDITED" && m.UpdateTime > 0 {
		out.Set("edited", time.UnixMilli(m.UpdateTime).UTC().Format(time.RFC3339))
	}
	return out
}

// ── отправка и правка ────────────────────────────────────────────────────

// maxText — текст черновика для MAX: Markdown → элементы.
func maxText(d *outbox.Draft) (string, []maxc.Element) {
	if d.Fmt == "plain" {
		return d.Text, nil
	}
	return maxc.Markdown(d.Text)
}

// maxDeliver — отправить или применить правку одобренного черновика.
func maxDeliver(ctx context.Context, s *config.Settings, rule config.ChatRule, d *outbox.Draft) (*omap.Map, error) {
	chatID, err := maxChatID(rule)
	if err != nil {
		return nil, err
	}
	// человек уже нажал «Отправить» — подождать связь подольше
	wctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	maxRT.Lock()
	cl := maxRT.client
	maxRT.Unlock()
	if cl == nil {
		return nil, ErrNoMax
	}
	c, err := cl.Conn(wctx)
	if err != nil {
		return nil, fmt.Errorf("нет связи с MAX: %w", err)
	}
	text, els := maxText(d)
	if d.EditOf != 0 {
		m, err := c.Edit(ctx, chatID, d.EditOf, text, els)
		if err != nil {
			return nil, err
		}
		return omap.New().Set("message_id", m.ID).Set("format", d.Fmt), nil
	}
	var reply int64
	if d.ReplyTo != nil {
		reply, _ = maxFixID(ctx, c, chatID, *d.ReplyTo)
	}
	attaches, err := maxUpload(ctx, c, d.Files)
	if err != nil {
		return nil, err
	}
	// разные виды вложений в одном сообщении сервер не принимает: фото —
	// вместе (с текстом), каждый файл, видео и голосовое — отдельно
	batches := [][]maxc.Attach{nil}
	for _, a := range attaches {
		if attachType(a) == "PHOTO" {
			batches[0] = append(batches[0], a)
		} else {
			batches = append(batches, []maxc.Attach{a})
		}
	}
	if text == "" && len(batches[0]) == 0 && len(batches) > 1 {
		batches = batches[1:] // без текста и фото — первым уходит первый файл
	}
	var ids []int64
	for i, b := range batches {
		o := maxc.SendOptions{Attaches: b}
		body := ""
		if i == 0 {
			body, o.Elements, o.ReplyTo = text, els, reply
		}
		m, err := c.Send(ctx, chatID, body, o)
		if err != nil {
			if len(ids) > 0 { // часть уже ушла — так и сказать
				return nil, fmt.Errorf("ушло %d сообщений из %d, дальше ошибка: %w", len(ids), len(batches), err)
			}
			return nil, err
		}
		markOutgoing(rule.Alias, int(m.ID))
		ids = append(ids, m.ID)
	}
	out := omap.New().Set("message_id", ids[0]).Set("format", d.Fmt)
	if len(ids) > 1 {
		out.Set("file_message_ids", ids[1:])
	}
	return out, nil
}

// maxEditOld — прежний текст своего сообщения для правки (и проверки: есть
// ли оно, своё ли, не прошло ли окно правки).
func maxEditOld(ctx context.Context, s *config.Settings, rule config.ChatRule, messageID int64) (string, error) {
	chatID, err := maxChatID(rule)
	if err != nil {
		return "", err
	}
	c, err := maxConn(ctx)
	if err != nil {
		return "", err
	}
	msgs, err := c.Messages(ctx, chatID, []int64{messageID})
	if err != nil {
		return "", err
	}
	if len(msgs) == 0 || msgs[0].Status == "REMOVED" {
		return "", &Bad{Msg: fmt.Sprintf("Сообщения %d больше нет (удалено) — править нечего.", messageID)}
	}
	m := msgs[0]
	maxRT.Lock()
	self := maxRT.self
	maxRT.Unlock()
	if m.Sender != self {
		return "", denied("Сообщение %d отправлено не с этого аккаунта.", messageID)
	}
	window := time.Duration(s.EditWindowMin) * time.Minute
	if age := time.Since(time.UnixMilli(m.Time)); age > window {
		return "", denied("Сообщение %d отправлено %s назад, а править своё можно %d мин после отправки. "+
			"Нужна поправка — напиши новым сообщением.", messageID, age.Round(time.Minute), s.EditWindowMin)
	}
	return m.Text, nil
}

// ── владельцу ────────────────────────────────────────────────────────────

// MaxDialog — чат аккаунта MAX для tg allow --match (только владельцу).
type MaxDialog struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	Kind   string `json:"kind"`
	Unread int    `json:"unread"`
}

// MaxDialogs — чаты из последнего входа; у личных — имя собеседника.
func MaxDialogs(ctx context.Context) ([]MaxDialog, error) {
	c, err := maxConn(ctx)
	if err != nil {
		return nil, err
	}
	maxRT.Lock()
	chats := make([]maxc.Chat, 0, len(maxRT.chats))
	for _, ch := range maxRT.chats {
		chats = append(chats, ch)
	}
	self := maxRT.self
	maxRT.Unlock()
	slices.SortFunc(chats, func(a, b maxc.Chat) int { return compareInt(b.LastEventTime, a.LastEventTime) })
	out := make([]MaxDialog, 0, len(chats))
	for _, ch := range chats {
		title := ch.Title
		if ch.Type == "DIALOG" && title == "" {
			for id := range ch.Participants {
				if id != self {
					title = maxName(ctx, c, id)
				}
			}
			if ch.ID == maxc.SavedChat || title == "" && len(ch.Participants) <= 1 {
				title = "Избранное"
			}
		}
		out = append(out, MaxDialog{ID: ch.ID, Title: title, Kind: ch.Type})
	}
	return out, nil
}

// maxSelfLabel — чей аккаунт MAX (для tg_list_chats при нескольких аккаунтах).
func maxSelfLabel() string {
	maxRT.Lock()
	defer maxRT.Unlock()
	if name := maxRT.names[maxRT.self]; name != "" {
		return name + " (MAX)"
	}
	if _, err := os.Stat(MaxSessionPath()); err != nil {
		return "MAX: входа нет"
	}
	return "MAX"
}

// MaxStatus — что видит служба MAX (tg max status при запущенной службе:
// второе соединение с тем же токеном не открываем).
type MaxStatus struct {
	Connected bool   `json:"connected"`
	Name      string `json:"name"`
	UserID    int64  `json:"user_id"`
	Chats     int    `json:"chats"`
}

func MaxState(ctx context.Context) (*MaxStatus, error) {
	maxRT.Lock()
	cl := maxRT.client
	st := &MaxStatus{Name: maxRT.names[maxRT.self], UserID: maxRT.self, Chats: len(maxRT.chats)}
	maxRT.Unlock()
	if cl == nil {
		return nil, ErrNoMax
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := cl.Conn(wctx)
	st.Connected = err == nil
	return st, nil
}

// maxNearestSent — точный id нашего отправленного сообщения по округлённому.
func maxNearestSent(s *config.Settings, alias string, id int64) int64 {
	drafts, err := outbox.New(s.OutboxPath()).List(outbox.Sent)
	if err != nil {
		return id
	}
	best, bestD := id, int64(maxIDSlack+1)
	for _, d := range drafts {
		if d.Chat != alias || d.MessageID == nil {
			continue
		}
		for _, cand := range []int64{*d.MessageID, d.EditOf} {
			if dist := abs64(cand - id); cand != 0 && dist < bestD {
				best, bestD = cand, dist
			}
		}
	}
	return best
}

// maxChatStatus — непрочитанные и последнее сообщение чатов белого списка
// (tg_list_chats with_status): из того, что служба знает о чатах.
func maxChatStatus(ctx context.Context, s *config.Settings) (map[string]*omap.Map, error) {
	c, err := maxConn(ctx)
	if err != nil {
		return nil, err
	}
	maxRT.Lock()
	chats := maps.Clone(maxRT.chats)
	maxRT.Unlock()
	out := map[string]*omap.Map{}
	for _, rule := range s.Rules() {
		id, err := maxChatID(rule)
		if err != nil {
			continue
		}
		ch, ok := chats[id]
		var at any
		preview := ""
		if ok && ch.LastMessage != nil {
			at = time.UnixMilli(ch.LastMessage.Time).UTC().Format(time.RFC3339)
			preview = truncate(ch.LastMessage.Text, 160)
		} else if hist, err := c.History(ctx, id, 0, 1); err == nil && len(hist) > 0 {
			at = time.UnixMilli(hist[len(hist)-1].Time).UTC().Format(time.RFC3339)
			preview = truncate(hist[len(hist)-1].Text, 160)
		}
		out[rule.Alias] = omap.New().Set("unread", ch.NewMessages).Set("unread_mentions", 0).
			Set("last_message_at", at).Set("last_message_preview", preview)
	}
	return out, nil
}

// maxTrack — событие обновляет то, что служба знает о чате: последнее
// сообщение и непрочитанные.
func maxTrack(ev maxc.Event) {
	if ev.Kind != maxc.EventNew && ev.Kind != maxc.EventRead {
		return
	}
	maxRT.Lock()
	defer maxRT.Unlock()
	if maxRT.chats == nil {
		return
	}
	ch, ok := maxRT.chats[ev.ChatID]
	if !ok {
		ch = maxc.Chat{ID: ev.ChatID}
	}
	if ev.Kind == maxc.EventNew && ev.Message != nil {
		m := *ev.Message
		ch.LastMessage, ch.LastEventTime = &m, m.Time
	}
	ch.NewMessages = ev.Unread
	maxRT.chats[ev.ChatID] = ch
}

// maxMediaKind — вид вложения словами Telegram (агенту привычно).
func maxMediaKind(a maxc.Attach) string {
	switch attachType(a) {
	case "PHOTO":
		return "photo"
	case "VIDEO":
		if attachInt(a, "videoType") == 1 {
			return "video_note"
		}
		return "video"
	case "AUDIO":
		return "voice"
	case "FILE":
		return "document"
	case "STICKER":
		return "sticker"
	case "SHARE":
		return "webpage"
	case "CONTACT":
		return "contact"
	case "CALL":
		return "call"
	case "POLL":
		return "poll"
	}
	return strings.ToLower(attachType(a))
}
