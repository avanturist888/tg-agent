package core

// Аккаунт WhatsApp (профиль с TG_NETWORK=whatsapp, internal/wac). Служба
// такого профиля держит связанное устройство WhatsApp: всё, что приходит, —
// в хранилище сообщений (истории на сервере WhatsApp нет), новое и правки —
// в ленты и подписки. Белый список, черновики, карточки и подписки — общие;
// здесь — то, что ходит в саму сеть. Id чатов и сообщений агенту — номера
// строк хранилища.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tgagent/internal/accounts"
	"tgagent/internal/config"
	"tgagent/internal/fsx"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/tgc"
	"tgagent/internal/upstream"
	"tgagent/internal/wac"
)

// WADir — устройство и сообщения WhatsApp профиля (data/wa).
func WADir() string { return filepath.Join(config.DataPath(), "wa") }

// waRT — WhatsApp в службе профиля. Хранилище открыто один раз на процесс
// и не закрывается: соединения приходят и уходят, а чтение, ссылки на
// клиента и фоновые дела не должны наткнуться на закрытую базу.
var waRT struct {
	sync.Mutex
	store   *wac.Store  // хранилище процесса (nil — служба WhatsApp не работает)
	client  *wac.Client // nil — соединения нет
	running bool        // служба WhatsApp работает
	login   *WALogin    // идущий (или только что законченный) вход
}

// ErrNoWA — без службы WhatsApp недоступен: соединение держит только она.
var ErrNoWA = errors.New("Служба аккаунта WhatsApp не запущена (задача Планировщика профиля) — без неё WhatsApp недоступен.")

func waClient() (*wac.Client, error) {
	waRT.Lock()
	defer waRT.Unlock()
	if waRT.client == nil {
		if waRT.running {
			return nil, &Bad{Msg: "Вход в WhatsApp не выполнен: связать устройство может только владелец — tg --profile " +
				config.Profile() + " wa login."}
		}
		return nil, ErrNoWA
	}
	return waRT.client, nil
}

// WALogin — ход входа (связки устройства) для tg wa login.
type WALogin struct {
	Stage string    `json:"stage"` // idle, starting, qr, code, done, failed
	QR    string    `json:"qr,omitempty"`
	Code  string    `json:"code,omitempty"`
	Until time.Time `json:"until,omitempty"` // до когда действует QR
	Phone string    `json:"phone,omitempty"` // связанный номер (done)
	Error string    `json:"error,omitempty"`
	// заказ
	reqPhone string
	force    bool
	at       time.Time
}

const waLoginWait = 10 * time.Minute

func waLoginUpdate(f func(l *WALogin)) {
	waRT.Lock()
	defer waRT.Unlock()
	if waRT.login != nil {
		f(waRT.login)
	}
}

// WALoginStart — начать вход в службе, её же соединением: история, которую
// телефон шлёт сразу после связки, должна лечь в хранилище, а второе
// соединение того же устройства выбило бы первое. force — отвязать прежнее.
func WALoginStart(ctx context.Context, phone string, force bool) (*WALogin, error) {
	waRT.Lock()
	if !waRT.running {
		waRT.Unlock()
		return nil, ErrNoWA
	}
	if l := waRT.login; l != nil && l.Stage != "done" && l.Stage != "failed" && time.Since(l.at) < waLoginWait {
		cp := *l
		waRT.Unlock()
		return &cp, nil // вход уже идёт — показать его
	}
	cl := waRT.client
	waRT.Unlock()
	linked := wac.HasSession(ctx, WADir())
	if linked && !force {
		out := &WALogin{Stage: "done"}
		if cl != nil {
			out.Phone = cl.Self().User
		}
		return out, nil
	}
	waRT.Lock()
	waRT.login = &WALogin{Stage: "starting", reqPhone: phone, force: force && linked, at: time.Now()}
	waRT.Unlock()
	if linked && cl != nil {
		if err := cl.Logout(ctx); err != nil { // Run сотрёт устройство, цикл начнёт вход
			waLoginUpdate(func(l *WALogin) { l.Stage, l.Error = "failed", err.Error() })
			return nil, fmt.Errorf("прежнее устройство не отвязалось: %w", err)
		}
	}
	return WALoginState(), nil
}

// WALoginState — где сейчас вход.
func WALoginState() *WALogin {
	waRT.Lock()
	defer waRT.Unlock()
	if waRT.login == nil {
		return &WALogin{Stage: "idle"}
	}
	cp := *waRT.login
	return &cp
}

// waNet — WhatsApp для общего кода (net.go).
type waNet struct{}

func (waNet) name() string { return "WhatsApp" }

// ruleChatID — номер чата из белого списка; чат слили с другим (одна личка
// под номером и под LID) — номер того, с которым слит.
func (waNet) ruleChatID(rule config.ChatRule) (int64, error) {
	var id int64
	switch p := rule.Peer.(type) {
	case int64:
		id = p
	case string:
		id, _ = strconv.ParseInt(p, 10, 64)
	}
	if id > 0 {
		waRT.Lock()
		st := waRT.store
		waRT.Unlock()
		if st != nil {
			id = st.Resolve(context.Background(), id)
		}
		return id, nil
	}
	return 0, &Bad{Msg: fmt.Sprintf("У чата '%s' в белом списке нет номера чата WhatsApp (id из tg_folder_chats).", rule.Alias)}
}

func waRule(s *config.Settings, chatID int64) (config.ChatRule, bool) {
	for _, r := range s.Rules() {
		if id, err := (waNet{}).ruleChatID(r); err == nil && id == chatID {
			return r, true
		}
	}
	return config.ChatRule{}, false
}

// waChat — чат белого списка в хранилище.
func waChat(ctx context.Context, rule config.ChatRule) (*wac.Client, wac.Chat, error) {
	c, err := waClient()
	if err != nil {
		return nil, wac.Chat{}, err
	}
	id, err := (waNet{}).ruleChatID(rule)
	if err != nil {
		return nil, wac.Chat{}, err
	}
	ch, err := c.Store.Chat(ctx, id)
	if err != nil {
		return nil, wac.Chat{}, &Bad{Msg: fmt.Sprintf("Чата %d нет среди чатов WhatsApp, которые видел шлюз.", id)}
	}
	return c, ch, nil
}

// ── служба ───────────────────────────────────────────────────────────────

func (waNet) run(ctx context.Context, loader func() (*config.Settings, error)) {
	log := func(event string, kv ...any) {
		if s, err := loader(); err == nil {
			logEvent(s, event, kv...)
		}
	}
	if err := os.MkdirAll(WADir(), 0o700); err != nil {
		log("wa_run_failed", "error", err.Error())
		return
	}
	st, err := wac.OpenStore(filepath.Join(WADir(), "messages.db"))
	if err != nil {
		log("wa_run_failed", "error", err.Error())
		return
	}
	waRT.Lock()
	waRT.running, waRT.store = true, st
	waRT.Unlock()
	defer func() {
		waRT.Lock()
		waRT.running = false
		waRT.Unlock()
	}()
	warned := false
	var outdated atomic.Bool // о версии клиента сказано — до следующего удачного входа
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		linked := wac.HasSession(ctx, WADir())
		waRT.Lock()
		login := waRT.login
		if login != nil && (login.Stage == "done" || login.Stage == "failed" || time.Since(login.at) > waLoginWait) {
			login = nil
		}
		waRT.Unlock()
		if !linked && login == nil {
			sleepCtx(ctx, 2*time.Second) // ждём входа владельца (tg wa login)
			continue
		}
		// своё время жизни у каждого соединения: его фоновые дела кончаются с ним
		cctx, cancel := context.WithCancel(ctx)
		cl := &wac.Client{Dir: WADir(), Store: st}
		var timer *time.Timer
		if !linked {
			cl.Pair = &wac.Pair{
				Phone: login.reqPhone,
				OnQR: func(code string, valid time.Duration) {
					waLoginUpdate(func(l *WALogin) { l.Stage, l.QR, l.Until = "qr", code, time.Now().Add(valid) })
				},
				OnCode: func(code string) { waLoginUpdate(func(l *WALogin) { l.Stage, l.Code = "code", code }) },
				OnPaired: func(phone string) {
					waLoginUpdate(func(l *WALogin) { l.Stage, l.Phone, l.QR, l.Code = "done", phone, "", "" })
					log("wa_login", "phone", phoneTail(phone))
				},
			}
			// не связали за отведённое время — бросить; связали — соединение остаётся
			timer = time.AfterFunc(waLoginWait, func() {
				if WALoginState().Stage != "done" {
					cancel()
				}
			})
		}
		cl.OnLog = func(event string, kv ...any) {
			log(event, kv...)
			if event == "wa_connected" {
				outdated.Store(false) // пустили — о следующем отказе снова скажем
				go waConnected(cctx, cl, loader)
			}
		}
		cl.OnEvent = func(ev wac.Event) { waOnEvent(loader, cl, ev) }
		waRT.Lock()
		waRT.client = cl
		waRT.Unlock()
		started := time.Now()
		err := cl.Run(cctx)
		cancel()
		if timer != nil {
			timer.Stop()
		}
		waRT.Lock()
		waRT.client = nil
		waRT.Unlock()
		if !linked {
			waLoginUpdate(func(l *WALogin) {
				if l.Stage != "done" {
					l.Stage, l.Error = "failed", "время вышло — вход не подтвердили"
					if err != nil {
						l.Error = err.Error()
					}
				}
			})
		}
		if ctx.Err() != nil {
			return
		}
		relink := WALoginState().force && WALoginState().Stage == "starting"
		switch {
		case errors.Is(err, wac.ErrLoggedOut) && relink:
			log("wa_relink") // отвязали сами, чтобы связать заново
			continue
		case errors.Is(err, wac.ErrLoggedOut):

			log("wa_session_revoked")
			if s, lerr := loader(); lerr == nil && s.BotReady() && !warned {
				warned = true
				_ = sendPinned(ctx, s, "⚠️ <b>WhatsApp отвязал шлюз</b>\n\nУстройство tg-agent больше не связано с аккаунтом "+
					"(выход на телефоне в «Связанных устройствах»). Агентам WhatsApp недоступен.\n\n"+
					"Связать заново: <code>tg --profile "+config.Profile()+" wa login</code>", "wa-session", "", "")
			}
		case errors.Is(err, wac.ErrNoSession):
		case errors.Is(err, wac.ErrClientOutdated):
			// чинит агент проекта (свежая whatsmeow), не владелец: вход цел
			if !outdated.Load() {
				if s, lerr := loader(); lerr == nil {
					outdated.Store(reportWAOutdated(ctx, s))
				}
			}
		case err != nil:
			log("wa_run_failed", "error", err.Error())
		}
		if err == nil || errors.Is(err, wac.ErrNoSession) {
			warned = false
		}
		if time.Since(started) > 5*time.Minute {
			backoff = 5 * time.Second
		}
		sleepCtx(ctx, backoff)
		backoff = min(backoff*2, 5*time.Minute)
	}
}

// waConnected — после подключения: названия групп, ленты догоняют то, что
// пришло без связи.
func waConnected(ctx context.Context, c *wac.Client, loader func() (*config.Settings, error)) {
	s, err := loader()
	if err != nil {
		return
	}
	if n, err := c.SyncGroups(ctx); err != nil {
		logEvent(s, "wa_groups_failed", "error", err.Error())
	} else {
		logEvent(s, "wa_groups", "count", n)
	}
	if err := c.SyncAppState(ctx); err != nil {
		logEvent(s, "wa_appstate_failed", "error", err.Error())
	}
	(waNet{}).catchup(ctx, s)
}

func waOnEvent(loader func() (*config.Settings, error), c *wac.Client, ev wac.Event) {
	s, err := loader()
	if err != nil {
		return
	}
	rule, ok := waRule(s, ev.Chat.ID)
	if !ok || !rule.Read {
		return // чат не в белом списке — его событий шлюз не касается
	}
	ids := make([]int, len(ev.IDs))
	for i, id := range ev.IDs {
		ids[i] = int(id)
	}
	switch ev.Kind {
	case wac.EventNew:
		// заметки себе (чат с собой) от владельца будят, как «Избранное»;
		// в остальных чатах своё — не новость
		if ev.FromMe && !c.IsSelfChat(ev.Chat) {
			for _, id := range ids {
				markOutgoing(rule.Alias, id)
			}
		}
		if _, err := appendNewer(s, rule.Alias, ids); err != nil {
			logEvent(s, "feed_failed", "chat", rule.Alias, "error", err.Error())
		}
	case wac.EventEdit:
		if ev.FromMe && !c.IsSelfChat(ev.Chat) {
			return // свою правку владелец знает сам; подписки — о чужом
		}
		noteChange(s, rule.Alias, ids, false)
	case wac.EventDelete:
		noteChange(s, rule.Alias, ids, true)
	}
}

func (waNet) catchup(ctx context.Context, s *config.Settings) {
	c, err := waClient()
	if err != nil {
		return
	}
	for _, rule := range s.Rules() {
		id, err := (waNet{}).ruleChatID(rule)
		if !rule.Read || err != nil {
			continue
		}
		// только живые: история при входе — не новое, подписки она не будит
		msgs, err := c.Store.History(ctx, id, wac.Query{Live: true, Limit: 30})
		if err != nil {
			continue
		}
		ch, _ := c.Store.Chat(ctx, id)
		ids := make([]int, 0, len(msgs))
		for _, m := range msgs {
			if m.FromMe && !c.IsSelfChat(ch) {
				markOutgoing(rule.Alias, int(m.ID))
			}
			ids = append(ids, int(m.ID))
		}
		slices.Sort(ids) // в хранилище по времени; ленте нужны номера по возрастанию
		if _, err := appendNewer(s, rule.Alias, ids); err != nil {
			logEvent(s, "feed_failed", "chat", rule.Alias, "error", err.Error())
		}
	}
}

// ── чтение ───────────────────────────────────────────────────────────────

func (waNet) readChat(ctx context.Context, s *config.Settings, rule config.ChatRule, o ReadOpts, limit int) ([]*omap.Map, error) {
	if o.Topic != 0 {
		return nil, &Bad{Msg: "В WhatsApp тем нет — читай без topic."}
	}
	c, ch, err := waChat(ctx, rule)
	if err != nil {
		return nil, err
	}
	var msgs []wac.Message
	if len(o.IDs) > 0 {
		for _, id := range o.IDs {
			if m, err := c.Store.Get(ctx, int64(id)); err == nil && m.Chat == ch.ID && !m.Deleted {
				msgs = append(msgs, *m)
			}
		}
	} else {
		msgs, err = c.Store.History(ctx, ch.ID, wac.Query{Before: int64(o.BeforeID), After: int64(o.AfterID),
			Since: o.Since, Search: o.Search, Limit: limit})
		if err != nil {
			return nil, &Bad{Msg: err.Error()}
		}
	}
	out := make([]*omap.Map, len(msgs))
	for i := range msgs {
		out[i] = waSerialize(ctx, c, &msgs[i], true)
	}
	if o.Transcribe {
		waAttachTranscripts(ctx, s, rule, c, msgs, out)
	}
	return out, nil
}

func waSerialize(ctx context.Context, c *wac.Client, m *wac.Message, withReply bool) *omap.Map {
	out := omap.New().Set("id", m.ID).Set("date", m.Time.UTC().Format(time.RFC3339))
	from := strings.TrimSuffix(m.Sender, "@s.whatsapp.net")
	out.Set("from", omap.New().Set("id", from).Set("name", c.Name(ctx, m.Sender, m.PushName)))
	if m.FromMe {
		out.Set("out", true)
	}
	out.Set("text", m.Text)
	if m.ReplyTo != "" {
		if r, err := c.Store.ByWAID(ctx, m.Chat, m.ReplyTo); err == nil {
			out.Set("reply_to", r.ID)
			if withReply && !r.Deleted {
				out.Set("reply_to_message", waSerialize(ctx, c, r, false))
			}
		}
	}
	if m.Forward {
		out.Set("forwarded_from", omap.New())
	}
	if m.Kind != "" {
		out.Set("media", m.Kind) // скачать — tg_download_file, картинку — tg_view_media
		if md, _, err := wac.MediaInfo(m); err == nil {
			if md.Kind == "document" {
				out.Set("file_name", md.Name)
			}
			if md.Size > 0 {
				out.Set("file_size", md.Size)
			}
			if md.Seconds > 0 {
				out.Set("duration", md.Seconds)
			}
			if md.MIME != "" {
				out.Set("mime_type", md.MIME)
			}
		}
	}
	if len(m.Reactions) > 0 {
		self := c.Self().String()
		counts := map[string]int{}
		var order []string
		mine := ""
		for who, e := range m.Reactions {
			if counts[e] == 0 {
				order = append(order, e)
			}
			counts[e]++
			if who == self {
				mine = e
			}
		}
		slices.Sort(order)
		list := make([]*omap.Map, 0, len(order))
		for _, e := range order {
			list = append(list, omap.New().Set("emoji", e).Set("count", counts[e]).Set("mine", e == mine))
		}
		out.Set("reactions", list)
	}
	if !m.Edited.IsZero() {
		out.Set("edited", m.Edited.UTC().Format(time.RFC3339))
	}
	return out
}

// ── отправка и правка ────────────────────────────────────────────────────

func (waNet) deliver(ctx context.Context, s *config.Settings, rule config.ChatRule, d *outbox.Draft) (*omap.Map, error) {
	// человек уже нажал «Отправить» — подождать связь подольше
	wctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	c, ch, err := waChat(wctx, rule)
	if err != nil {
		return nil, err
	}
	text := d.Text
	if d.Fmt != "plain" {
		text = wac.FromMarkdown(text)
	}
	if d.EditOf != 0 {
		m, err := c.Store.Get(wctx, d.EditOf)
		if err != nil {
			return nil, &Bad{Msg: fmt.Sprintf("Сообщения %d нет в хранилище — править нечего.", d.EditOf)}
		}
		if err := c.Edit(wctx, m, text); err != nil {
			return nil, err
		}
		return omap.New().Set("message_id", m.ID).Set("format", d.Fmt), nil
	}
	var reply *wac.Message
	if d.ReplyTo != nil {
		if r, err := c.Store.Get(wctx, *d.ReplyTo); err == nil && r.Chat == ch.ID {
			reply = r
		}
	}
	// один файл с подписью — одним сообщением; иначе текст, потом файлы
	caption := len(d.Files) == 1 && text != ""
	var ids []int64
	sent := func(m *wac.Message) {
		markOutgoing(rule.Alias, int(m.ID))
		ids = append(ids, m.ID)
	}
	// часть уже ушла (или ушло, но не записалось) — повтор всего черновика
	// продублировал бы её у получателя: не повторять, так и сказать
	partial := func(err error) error {
		var unsaved *wac.SentUnsaved
		if errors.As(err, &unsaved) {
			return &Denied{Msg: err.Error() + " (повторять не стал, чтобы не продублировать)"}
		}
		if len(ids) > 0 {
			return &Denied{Msg: fmt.Sprintf("ушло %d сообщений, дальше ошибка (повторять не стал, чтобы не продублировать): %v", len(ids), err)}
		}
		return err
	}
	if text != "" && !caption {
		m, err := c.SendText(wctx, ch, text, reply)
		if err != nil {
			return nil, partial(err)
		}
		sent(m)
		reply = nil
	}
	for i, f := range d.Files {
		cap := ""
		if caption && i == 0 {
			cap = text
		}
		m, err := c.SendFile(wctx, ch, wac.File{Path: f.Path, Name: f.Name, Photo: f.Kind == outbox.KindPhoto}, cap, reply)
		if err != nil {
			return nil, partial(err)
		}
		sent(m)
		reply = nil
	}
	if len(ids) == 0 {
		return nil, &Bad{Msg: "Нечего отправлять: ни текста, ни файлов."}
	}
	feed := make([]int, len(ids))
	for i, id := range ids {
		feed[i] = int(id)
	}
	_, _ = appendNewer(s, rule.Alias, feed)
	out := omap.New().Set("message_id", ids[0]).Set("format", d.Fmt)
	if len(ids) > 1 {
		out.Set("file_message_ids", ids[1:])
	}
	return out, nil
}

func (waNet) editOld(ctx context.Context, s *config.Settings, rule config.ChatRule, messageID int64) (string, error) {
	c, ch, err := waChat(ctx, rule)
	if err != nil {
		return "", err
	}
	m, err := c.Store.Get(ctx, messageID)
	if err != nil || m.Chat != ch.ID {
		return "", &Bad{Msg: fmt.Sprintf("Сообщения %d нет в этом чате.", messageID)}
	}
	if m.Deleted {
		return "", &Bad{Msg: fmt.Sprintf("Сообщения %d больше нет (удалено) — править нечего.", messageID)}
	}
	if !m.FromMe {
		return "", denied("Сообщение %d отправлено не с этого аккаунта.", messageID)
	}
	if m.Kind != "" && !slices.Contains([]string{"photo", "video", "document"}, m.Kind) {
		return "", denied("У сообщения %d (%s) нет текста, который можно поправить.", messageID, m.Kind)
	}
	window := time.Duration(s.EditWindowMin) * time.Minute
	if age := time.Since(m.Time); age > window {
		return "", denied("Сообщение %d отправлено %s назад, а править своё можно %d мин после отправки. "+
			"Нужна поправка — напиши новым сообщением.", messageID, age.Round(time.Minute), s.EditWindowMin)
	}
	return m.Text, nil
}

func (waNet) react(ctx context.Context, rule config.ChatRule, messageID int64, emoji string) ([]*omap.Map, error) {
	c, ch, err := waChat(ctx, rule)
	if err != nil {
		return nil, err
	}
	m, err := c.Store.Get(ctx, messageID)
	if err != nil || m.Chat != ch.ID || m.Deleted {
		return nil, &Bad{Msg: fmt.Sprintf("Сообщения %d нет в этом чате.", messageID)}
	}
	if err := c.React(ctx, m, emoji); err != nil {
		return nil, &Bad{Msg: "WhatsApp отказал: " + err.Error()}
	}
	m, err = c.Store.Get(ctx, messageID)
	if err != nil {
		return nil, err
	}
	list, _ := waSerialize(ctx, c, m, false).Get("reactions")
	if l, ok := list.([]*omap.Map); ok {
		return l, nil
	}
	return []*omap.Map{}, nil
}

// ── вложения ─────────────────────────────────────────────────────────────

func waMessage(ctx context.Context, rule config.ChatRule, messageID int64) (*wac.Client, *wac.Message, error) {
	c, ch, err := waChat(ctx, rule)
	if err != nil {
		return nil, nil, err
	}
	m, err := c.Store.Get(ctx, messageID)
	if err != nil || m.Chat != ch.ID || m.Deleted {
		return nil, nil, &Bad{Msg: fmt.Sprintf("Сообщения %d нет в этом чате.", messageID)}
	}
	return c, m, nil
}

// waDownload — вложение в dir (уже скачанное — не качаем снова).
func waDownload(ctx context.Context, c *wac.Client, m *wac.Message, dir string, limit int64) (*tgc.Downloaded, error) {
	info, _, err := wac.MediaInfo(m)
	if err != nil {
		return nil, &Bad{Msg: fmt.Sprintf("В сообщении %d нет файла.", m.ID)}
	}
	got := &tgc.Downloaded{Name: safeName(info.Name)}
	if info.Seconds > 0 {
		sec := info.Seconds
		got.Duration = &sec
	}
	got.Path = filepath.Join(dir, strconv.FormatInt(m.ID, 10)+"-"+got.Name)
	if st, err := os.Stat(got.Path); err == nil && st.Size() > 0 {
		got.Size, got.Cached, got.MIME = st.Size(), true, info.MIME
		return got, nil
	}
	md, err := c.Download(ctx, m, limit)
	if err != nil {
		return nil, &Bad{Msg: err.Error()}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := fsx.WriteFile(got.Path, md.Data, 0o600); err != nil {
		return nil, err
	}
	got.Size, got.MIME = md.Size, md.MIME
	if got.MIME == "" {
		got.MIME = http.DetectContentType(md.Data)
	}
	return got, nil
}

func (waNet) downloadFile(ctx context.Context, s *config.Settings, rule config.ChatRule, messageID int64) (*omap.Map, error) {
	c, m, err := waMessage(ctx, rule, messageID)
	if err != nil {
		return nil, err
	}
	got, err := waDownload(ctx, c, m, filepath.Join(s.DownloadsDir(), rule.Alias), int64(s.MaxDownloadMB)*1024*1024)
	if err != nil {
		return nil, err
	}
	logEvent(s, "download", "chat", rule.Alias, "message_id", m.ID, "name", got.Name, "size", got.Size)
	return omap.New().Set("chat", s.Ref(rule.Alias)).Set("message_id", m.ID).Set("path", got.Path).
		Set("name", got.Name).Set("mime_type", got.MIME).Set("size", got.Size).Set("duration", durVal(got.Duration)).
		Set("cached", got.Cached), nil
}

func (waNet) viewMedia(ctx context.Context, s *config.Settings, rule config.ChatRule, messageID int64) (*omap.Map, *tgc.Image, error) {
	c, m, err := waMessage(ctx, rule, messageID)
	if err != nil {
		return nil, nil, err
	}
	info, _, err := wac.MediaInfo(m)
	if err != nil {
		return nil, nil, &Bad{Msg: fmt.Sprintf("В сообщении %d нет картинки.", m.ID)}
	}
	var (
		data []byte
		kind = "photo"
	)
	switch m.Kind {
	case "photo", "sticker":
		kind = m.Kind
		got, err := waDownload(ctx, c, m, filepath.Join(s.DownloadsDir(), rule.Alias), 20<<20)
		if err != nil {
			if len(info.Thumb) == 0 {
				return nil, nil, err
			}
			data, kind = info.Thumb, "thumbnail" // файла на сервере уже нет — хотя бы превью
		} else if data, err = os.ReadFile(got.Path); err != nil {
			return nil, nil, err
		}
	default:
		if len(info.Thumb) == 0 {
			return nil, nil, &Bad{Msg: fmt.Sprintf("В сообщении %d (%s) нет картинки; файл — tg_download_file.", m.ID, m.Kind)}
		}
		data, kind = info.Thumb, "preview_frame"
	}
	format := strings.TrimPrefix(http.DetectContentType(data), "image/")
	if !slices.Contains([]string{"jpeg", "png", "gif", "webp"}, format) {
		return nil, nil, &Bad{Msg: "Вложение — не картинка, которую можно показать (" + format + "); скачай его через tg_download_file."}
	}
	logEvent(s, "view_media", "chat", rule.Alias, "message_id", m.ID, "kind", kind)
	meta := omap.New().Set("chat", s.Ref(rule.Alias)).Set("message_id", m.ID).Set("kind", kind).
		Set("caption", m.Text).Set("bytes", len(data))
	return meta, &tgc.Image{Data: data, Format: format, Kind: kind, Caption: m.Text}, nil
}

// waTranscribe — расшифровка голосового или кружка своей моделью (у
// WhatsApp серверной расшифровки для сторонних устройств нет).
func waTranscribe(ctx context.Context, s *config.Settings, rule config.ChatRule, c *wac.Client, m *wac.Message, engine string) (tgc.Transcript, *float64, error) {
	if m.Kind != "voice" && m.Kind != "video_note" && m.Kind != "audio" {
		return tgc.Transcript{}, nil, &Bad{Msg: fmt.Sprintf("Сообщение %d — не голосовое и не кружок.", m.ID)}
	}
	var dur *float64
	if info, _, err := wac.MediaInfo(m); err == nil && info.Seconds > 0 {
		sec := info.Seconds
		dur = &sec
	}
	cache := tgc.TranscriptCache{Dir: s.DataDir()}
	key := fmt.Sprintf("%s:%d", rule.Alias, m.ID)
	if cached, ok := cache.Get(key); ok {
		return cached, dur, nil
	}
	local := localSTT(s, rule)
	if local.Local == nil {
		return tgc.Transcript{}, dur, &Bad{Msg: "Расшифровка WhatsApp — только своя (GigaAM), а она на этой машине не установлена " +
			"(scripts\\install-stt.ps1). Голосовое можно скачать: tg_download_file."}
	}
	if engine == "gigaam" {
		local.Local = forcedRussian
	}
	file, err := waDownload(ctx, c, m, local.Dir, local.MaxBytes)
	if err != nil {
		return tgc.Transcript{}, dur, err
	}
	got, ok := tgc.LocalFile(ctx, file.Path, local)
	if !ok {
		msg := "своя расшифровка не вышла"
		if got.LocalError != "" {
			msg += ": " + got.LocalError
		}
		return got, dur, &Bad{Msg: msg + " (у WhatsApp своей расшифровки для шлюза нет)."}
	}
	cache.Put(key, got)
	return got, dur, nil
}

func (waNet) transcribe(ctx context.Context, s *config.Settings, rule config.ChatRule, messageID int64, engine string) (*omap.Map, error) {
	switch engine {
	case "", "auto", "gigaam":
	default:
		return nil, &Bad{Msg: "engine в WhatsApp: auto или gigaam (расшифровка только своя)."}
	}
	c, m, err := waMessage(ctx, rule, messageID)
	if err != nil {
		return nil, err
	}
	got, dur, err := waTranscribe(ctx, s, rule, c, m, engine)
	if err != nil {
		return nil, err
	}
	logEvent(s, "transcribe", "chat", rule.Alias, "message_id", m.ID, "status", got.Status, "engine", got.Engine)
	out := omap.New().Set("chat", s.Ref(rule.Alias)).Set("message_id", m.ID).Set("duration", durVal(dur))
	tgc.ApplyTranscript(out, got)
	return out, nil
}

// waAttachTranscripts — голосовые в выдаче чтения с расшифровкой (из кэша;
// новые — не больше TranscribePerCall за вызов, как в Telegram).
func waAttachTranscripts(ctx context.Context, s *config.Settings, rule config.ChatRule, c *wac.Client, msgs []wac.Message, out []*omap.Map) {
	budget := s.TranscribePerCall
	cache := tgc.TranscriptCache{Dir: s.DataDir()}
	for i := range msgs {
		if msgs[i].Kind != "voice" && msgs[i].Kind != "video_note" {
			continue
		}
		if cached, ok := cache.Get(fmt.Sprintf("%s:%d", rule.Alias, msgs[i].ID)); ok {
			tgc.ApplyTranscript(out[i], cached)
			continue
		}
		if budget <= 0 {
			out[i].Set("transcript_status", "not_requested").Set("transcript", nil)
			continue
		}
		budget--
		got, _, err := waTranscribe(ctx, s, rule, c, &msgs[i], "")
		if err != nil {
			out[i].Set("transcript_status", "error").Set("transcript", nil).Set("transcript_error", err.Error())
			continue
		}
		tgc.ApplyTranscript(out[i], got)
	}
}

// ── чаты, папки, доступ ──────────────────────────────────────────────────

// Папок WhatsApp у связанного устройства нет — вместо них три постоянные
// выборки: все чаты, лички, группы.
var waFolders = []struct{ id, title, rule string }{
	{"all", "Все чаты", "все чаты аккаунта WhatsApp"},
	{"personal", "Личные", "переписка с людьми"},
	{"groups", "Группы", "группы и сообщества"},
}

func waFolder(folder string) (id, title string, ok bool) {
	for _, f := range waFolders {
		if strings.EqualFold(folder, f.id) || strings.EqualFold(folder, f.title) {
			return f.id, f.title, true
		}
	}
	return "", "", false
}

func waInFolder(folder string, ch wac.Chat) bool {
	switch folder {
	case "personal":
		return ch.Kind == "user"
	case "groups":
		return ch.Kind == "group"
	}
	return true
}

func (waNet) listFolders(ctx context.Context, s *config.Settings) (*omap.Map, error) {
	if _, err := waClient(); err != nil {
		return nil, err
	}
	list := make([]*omap.Map, 0, len(waFolders))
	for _, f := range waFolders {
		list = append(list, omap.New().Set("id", f.id).Set("title", f.title).Set("rules", []string{f.rule}))
	}
	logEvent(s, "folders_listed", "count", len(list))
	return omap.New().Set("folders", list).Set("hint",
		"Чаты папки — tg_folder_chats(folder=<id или название>, account=\""+config.AccountName()+"\")."), nil
}

func waFolderList(ctx context.Context, folder string) (*wac.Client, string, string, []wac.Chat, error) {
	c, err := waClient()
	if err != nil {
		return nil, "", "", nil, err
	}
	id, title, ok := waFolder(folder)
	if !ok {
		var names []string
		for _, f := range waFolders {
			names = append(names, fmt.Sprintf("%s «%s»", f.id, f.title))
		}
		return nil, "", "", nil, &Bad{Msg: fmt.Sprintf("Папки «%s» нет. Есть: %s.", folder, strings.Join(names, ", "))}
	}
	all, err := c.Store.Chats(ctx)
	if err != nil {
		return nil, "", "", nil, err
	}
	var chats []wac.Chat
	for _, ch := range all {
		if waInFolder(id, ch) && ch.Kind != "broadcast" {
			chats = append(chats, ch)
		}
	}
	return c, id, title, chats, nil
}

func (waNet) folderChats(ctx context.Context, s *config.Settings, folder string) (*omap.Map, error) {
	c, id, title, chats, err := waFolderList(ctx, folder)
	if err != nil {
		return nil, err
	}
	known := map[int64]config.ChatRule{}
	for _, r := range s.Rules() {
		if cid, err := (waNet{}).ruleChatID(r); err == nil {
			known[cid] = r
		}
	}
	pending := map[int64]string{}
	for _, r := range loadAccess(s) {
		if r.Status == accessPending {
			pending[r.Peer] = r.ID
		}
	}
	rows := make([]*omap.Map, 0, len(chats))
	for _, ch := range chats {
		item := omap.New().Set("id", ch.ID).Set("title", c.ChatTitle(ctx, ch)).Set("kind", ch.Kind).Set("unread", ch.Unread)
		if rule, ok := known[ch.ID]; ok {
			item.Set("alias", s.Ref(rule.Alias)).Set("can_read", rule.Read).Set("can_send", rule.Send)
		} else {
			item.Set("alias", nil)
		}
		if rid, ok := pending[ch.ID]; ok {
			item.Set("access_request", rid)
		}
		rows = append(rows, item)
	}
	logEvent(s, "folder_chats_listed", "folder", title, "count", len(rows))
	return omap.New().Set("folder", omap.New().Set("id", id).Set("title", title)).
		Set("count", len(rows)).Set("chats", rows).Set("hint",
		"alias = null — чата нет в белом списке. Нужен доступ — tg_request_access(chat_id=id, account=\""+
			config.AccountName()+"\", reason=…)."), nil
}

func (waNet) folderSample(ctx context.Context, s *config.Settings, r *accessRequest, folder string) error {
	c, id, title, chats, err := waFolderList(ctx, folder)
	if err != nil {
		return err
	}
	r.Folder, r.Title, r.Count = id, title, len(chats)
	for i, ch := range chats {
		if i == 12 {
			break
		}
		r.Sample = append(r.Sample, c.ChatTitle(ctx, ch))
	}
	return nil
}

func (waNet) setMembers(ctx context.Context, s *config.Settings) (map[string]config.SetState, error) {
	c, err := waClient()
	if err != nil {
		return nil, err
	}
	all, err := c.Store.Chats(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]config.SetState{}
	for _, set := range s.Sets {
		if set.Public {
			out[set.Key] = config.SetState{Error: "в WhatsApp нет публичных каналов с @username — открой папку", Members: []config.SetMember{}}
			continue
		}
		name := set.Folder
		if name == "" {
			name = set.Name
		}
		id, title, ok := waFolder(name)
		if !ok {
			out[set.Key] = config.SetState{Error: "такой папки в WhatsApp нет (есть all, personal, groups)", Members: []config.SetMember{}}
			continue
		}
		state := config.SetState{Title: title, Members: []config.SetMember{}}
		for _, ch := range all {
			if waInFolder(id, ch) && ch.Kind != "broadcast" {
				state.Members = append(state.Members, config.SetMember{Peer: ch.ID, Title: c.ChatTitle(ctx, ch), Kind: ch.Kind})
			}
		}
		out[set.Key] = state
	}
	return out, nil
}

func (waNet) chatInfo(ctx context.Context, chatID int64) (tgc.DialogRow, error) {
	c, err := waClient()
	if err != nil {
		return tgc.DialogRow{}, err
	}
	ch, err := c.Store.Chat(ctx, chatID)
	if err != nil {
		return tgc.DialogRow{}, &Bad{Msg: fmt.Sprintf("Чат %d не найден среди чатов WhatsApp (tg_folder_chats — какие есть).", chatID)}
	}
	return tgc.DialogRow{ID: ch.ID, Title: c.ChatTitle(ctx, ch), Kind: ch.Kind, Unread: ch.Unread}, nil
}

func (waNet) dialogs(ctx context.Context) ([]tgc.DialogRow, error) {
	c, err := waClient()
	if err != nil {
		return nil, err
	}
	all, err := c.Store.Chats(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]tgc.DialogRow, 0, len(all))
	for _, ch := range all {
		if ch.Kind != "broadcast" {
			rows = append(rows, tgc.DialogRow{ID: ch.ID, Title: c.ChatTitle(ctx, ch), Kind: ch.Kind, Unread: ch.Unread})
		}
	}
	return rows, nil
}

func (waNet) chatStatus(ctx context.Context, s *config.Settings) (map[string]*omap.Map, error) {
	c, err := waClient()
	if err != nil {
		return nil, err
	}
	out := map[string]*omap.Map{}
	for _, rule := range s.Rules() {
		id, err := (waNet{}).ruleChatID(rule)
		if err != nil {
			continue
		}
		ch, err := c.Store.Chat(ctx, id)
		if err != nil {
			continue
		}
		var at any
		preview := ""
		if last, err := c.Store.History(ctx, id, wac.Query{Limit: 1}); err == nil && len(last) > 0 {
			at = last[0].Time.UTC().Format(time.RFC3339)
			preview = last[0].Text
			if preview == "" {
				preview = "[" + last[0].Kind + "]"
			}
			preview = truncate(preview, 160)
		}
		out[rule.Alias] = omap.New().Set("unread", ch.Unread).Set("unread_mentions", 0).
			Set("last_message_at", at).Set("last_message_preview", preview)
	}
	return out, nil
}

func (waNet) selfLabel() string {
	c, err := waClient()
	if err != nil || c.Self().IsEmpty() {
		if !wac.HasSession(context.Background(), WADir()) {
			return "WhatsApp: входа нет"
		}
		return "WhatsApp"
	}
	return c.Name(context.Background(), c.Self().String(), "") + " (WhatsApp)"
}

// WAStatus — что видит служба WhatsApp (tg wa status при запущенной службе:
// второе соединение того же устройства не открываем).
type WAStatus struct {
	Connected bool   `json:"connected"`
	Phone     string `json:"phone"`
	Name      string `json:"name"`
	Chats     int    `json:"chats"`
}

func WAState(ctx context.Context) (*WAStatus, error) {
	c, err := waClient()
	if err != nil {
		return nil, err
	}
	st := &WAStatus{Connected: c.Connected(), Phone: c.Self().User}
	if !wac.HasSession(ctx, WADir()) {
		st.Phone = "" // идёт вход: устройство ещё не связано
	}
	if !c.Self().IsEmpty() {
		st.Name = c.Name(ctx, c.Self().String(), "")
	}
	if chats, err := c.Store.Chats(ctx); err == nil {
		st.Chats = len(chats)
	}
	return st, nil
}

// phoneTail — последние цифры номера (в аудит — не весь номер).
func phoneTail(p string) string {
	if len(p) <= 4 {
		return p
	}
	return "…" + p[len(p)-4:]
}

// waOutdatedCard — карточка в бот: WhatsApp не пускает клиент шлюза.
func waOutdatedCard() string {
	return "⚠️ <b>WhatsApp отверг клиент шлюза как устаревший</b>\n\nСервер не пускает версию веб-клиента, " +
		"которой представляется whatsmeow (" + waVersion() + "). Вход цел — нужна свежая whatsmeow. " +
		"Пока её нет, агентам WhatsApp недоступен."
}

// waOutdatedBrief — то же для агента проекта: что случилось и что делать.
func waOutdatedBrief() string {
	return "Служба профиля " + config.ProfileName() + " (WhatsApp): сервер отверг подключение кодом 405 — " +
		"версия веб-клиента в whatsmeow (" + waVersion() + ", store/clientpayload.go) устарела. " +
		"Обнови whatsmeow до свежего коммита основной ветки; если и там версия старая — " +
		"store.SetWAVersion в internal/wac до подключения (текущую версию веб-клиента WhatsApp " +
		"проверь сам) и отметь это в выводе."
}

func waVersion() string { return wac.Version() }

// reportIncident — передать сбой основной службе (тесты подменяют).
var reportIncident = func(ctx context.Context, in UpstreamIncident) error {
	return accounts.Call(ctx, config.MainAccount, "upstream_incident", in, nil)
}

// reportWAOutdated — разбор отказа у агента проекта через слежение
// основного профиля; не дошло — карточка владельцу здесь, закреплённая
// (без разбора агентом). true — кто-то узнал.
func reportWAOutdated(ctx context.Context, s *config.Settings) bool {
	in := UpstreamIncident{Source: upstream.WhatsmeowCommits.ID, Card: waOutdatedCard(),
		Title: "WhatsApp отверг клиент как устаревший", Brief: waOutdatedBrief()}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	err := reportIncident(cctx, in)
	if err == nil {
		return true
	}
	logEvent(s, "wa_outdated_report_failed", "error", err.Error())
	if !s.BotReady() {
		return false
	}
	return sendPinned(cctx, s, in.Card, in.Source, in.Title, "") == nil
}
