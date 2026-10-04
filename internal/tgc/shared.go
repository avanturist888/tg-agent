package tgc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"tgagent/internal/audit"
	"tgagent/internal/config"
	"tgagent/internal/lock"
	"tgagent/internal/svc"
)

// Режим службы: одно постоянное соединение на весь процесс.
//
// Служба держит сессию всё время своей работы: без переподключения на
// каждый вызов и без лока на каждый запрос. Новые сообщения Telegram
// присылает сам (менеджер обновлений gotd с восстановлением пропусков),
// опрашивать ничего не нужно. Run в этом процессе просто отдаёт общее
// соединение; остальные процессы ходят в Telegram через службу (см. svc).

type sharedState struct {
	mu         sync.Mutex
	conn       *Conn
	authorized bool
	ready      chan struct{} // закрыт, пока соединение есть
	loggedIn   chan struct{} // сигнал: вход выполнен, можно запускать обновления
	login      *loginState
	cancel     context.CancelFunc // оборвать текущее соединение
	reset      bool               // при переподключении начать с чистой сессии (после выхода)
	keyKept    bool               // gotd пытался сменить ключ — переподключаемся со старым
	warned     bool               // владельцу уже сказали, что входа нет
}

var shared *sharedState

// EnableService — этот процесс становится службой: Run отдаёт общее
// соединение. Вызывать до приёма запросов, раньше Serve.
func EnableService() {
	shared = &sharedState{ready: make(chan struct{}), loggedIn: make(chan struct{}, 1)}
}

// InService — этот процесс и есть служба.
func InService() bool { return shared != nil }

func (sh *sharedState) set(c *Conn, authorized bool) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.conn, sh.authorized = c, authorized
	if authorized {
		sh.keyKept, sh.warned = false, false
	}
	if c != nil {
		select {
		case <-sh.ready:
		default:
			close(sh.ready)
		}
	} else {
		select {
		case <-sh.ready:
			sh.ready = make(chan struct{})
		default:
		}
	}
}

// wait — дождаться соединения (служба могла как раз переподключаться).
func (sh *sharedState) wait(ctx context.Context, timeout time.Duration) (*Conn, bool, error) {
	sh.mu.Lock()
	ready := sh.ready
	sh.mu.Unlock()
	select {
	case <-ready:
	case <-ctx.Done():
		return nil, false, ctx.Err()
	case <-time.After(timeout):
		sh.mu.Lock()
		kept := sh.keyKept
		sh.mu.Unlock()
		if kept {
			return nil, false, &lock.Busy{Msg: KeyRejected{}.Error()}
		}
		return nil, false, &lock.Busy{Msg: "Служба tg-agent сейчас не подключена к Telegram (сеть?) — повтори чуть позже."}
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.conn == nil {
		return nil, false, &lock.Busy{Msg: "Служба tg-agent переподключается к Telegram — повтори чуть позже."}
	}
	return sh.conn, sh.authorized, nil
}

func runShared(ctx context.Context, o Opts, fn func(ctx context.Context, c *Conn) error) error {
	c, authorized, err := shared.wait(ctx, 60*time.Second)
	if err != nil {
		return err
	}
	if !o.NoAuth && !authorized {
		return notLoggedIn()
	}
	err = fn(ctx, c)
	switch {
	case authLost(err):
		// ключ отозван (завершили сеанс, аккаунт заблокирован): переподключимся
		// и будем ждать входа
		shared.drop(c, false)
		return notLoggedIn()
	case notInited(err):
		// соединение живёт без initConnection (gotd сменил ключ на лету) —
		// каждый запрос будет падать, пока не переподключимся
		shared.drop(c, true)
		return &lock.Busy{Msg: "Служба tg-agent переподключается к Telegram — повтори чуть позже."}
	}
	return err
}

// drop — оборвать соединение c (если оно ещё текущее), чтобы служба
// переподключилась с ключом из файла.
func (sh *sharedState) drop(c *Conn, authorized bool) {
	sh.mu.Lock()
	cancel := sh.cancel
	if sh.conn != c {
		cancel = nil
	}
	sh.authorized = sh.authorized && authorized
	sh.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// authLost — Telegram больше не принимает этот ключ как вход в аккаунт.
func authLost(err error) bool {
	e, ok := tgerr.As(err)
	return ok && e.Code == 401 && e.Type != "SESSION_PASSWORD_NEEDED"
}

func notInited(err error) bool {
	e, ok := tgerr.As(err)
	return ok && e.IsOneOf("CONNECTION_NOT_INITED", "CONNECTION_LAYER_INVALID")
}

func notLoggedIn() error {
	return &NotLoggedIn{fmt.Sprintf("Нет валидной сессии Telegram. Человек должен войти: окно управления "+
		"(вкладка «Состояние») или `%s login`.", config.CLIHint())}
}

// ServiceOwned — сессию держит служба; вызов должен идти через неё.
type ServiceOwned struct{}

func (ServiceOwned) Error() string {
	return "Сессию Telegram держит служба tg-agent — этот вызов должен идти через неё."
}

func serviceUp(ctx context.Context) bool { return !InService() && svc.Up(ctx) }

// OnMessage — новое сообщение в любом чате аккаунта (peer, id сообщения;
// out — исходящее, от владельца или агента).
type OnMessage func(ctx context.Context, c *Conn, peer tg.PeerClass, msgID int, out bool)

// Events — что служба делает с событиями чатов. Пустые поля — не нужно.
type Events struct {
	Message OnMessage // новое сообщение
	Edit    OnMessage // видимая правка сообщения (не реакции и не служебные)
	// Delete — сообщения удалены. peer == nil — лички и обычные группы:
	// Telegram не говорит, где они были (id там общие на весь аккаунт).
	Delete func(ctx context.Context, c *Conn, peer tg.PeerClass, ids []int)
}

// Serve — держать постоянное соединение, пока не отменят ctx. Переподключается
// сам. ev получает новые, изменённые и удалённые сообщения; onReady вызывается после
// каждого (пере)подключения с действующим входом — чтобы догнать пропущенное;
// onNoLogin — один раз за раз, когда Telegram не принимает сессию (нужен вход).
func Serve(ctx context.Context, s *config.Settings, ev Events, onReady func(ctx context.Context, c *Conn), onNoLogin func(ctx context.Context)) error {
	if shared == nil {
		EnableService()
	}
	// сессию держим всё время работы: прямые подключения других процессов
	// (если служба их не заметит) подождут, а не подерутся с нами
	flock := lock.New(s.SessionPath, 30*time.Second)
	if err := flock.Acquire(ctx); err != nil {
		return err
	}
	defer flock.Release()
	go func() {
		for ctx.Err() == nil {
			flock.Touch()
			sleep(ctx, time.Minute)
		}
	}()

	backoff := 2 * time.Second
	for ctx.Err() == nil {
		shared.mu.Lock()
		reset := shared.reset
		shared.reset = false
		shared.mu.Unlock()
		if reset {
			// после выхода ключ отозван — новая сессия с чистого листа
			_ = os.Remove(s.SessionPath)
			_ = os.Remove(s.SessionPath + ".keyloss")
		}
		started := time.Now()
		octx, cancel := context.WithCancel(ctx)
		shared.mu.Lock()
		shared.cancel = cancel
		shared.mu.Unlock()
		var kept atomic.Bool
		storage := newGuardedStorage(s, false, func() {
			kept.Store(true)
			shared.mu.Lock()
			shared.keyKept = true
			shared.mu.Unlock()
			cancel() // в памяти клиента уже новый ключ — начинаем с файла
		})
		err := serveOnce(octx, s, storage, ev, onReady, onNoLogin)
		cancel()
		shared.set(nil, false)
		if ctx.Err() != nil {
			return nil
		}
		if !kept.Load() && (reset || errors.Is(err, context.Canceled)) {
			continue // переподключение по нашей же просьбе
		}
		if !kept.Load() {
			_ = audit.Log(s.AuditPath(), "service_disconnected", "error", fmt.Sprint(err))
		}
		if time.Since(started) > 5*time.Minute {
			backoff = 2 * time.Second // долго работало — сбой разовый
		}
		sleep(ctx, backoff)
		backoff = min(backoff*2, time.Minute)
	}
	return nil
}

func serveOnce(ctx context.Context, s *config.Settings, storage session.Storage, ev Events,
	onReady func(ctx context.Context, c *Conn), onNoLogin func(ctx context.Context)) error {
	c := &Conn{Settings: s, hub: newUpdateHub(), Peers: LoadPeers(s.PeersPath())}
	d := tg.NewUpdateDispatcher()
	message := func(handler OnMessage, e tg.Entities, m tg.MessageClass) func(ctx context.Context) {
		return func(ctx context.Context) {
			c.Peers.RememberEntities(e)
			if peer := messagePeer(m); peer != nil && handler != nil {
				handler(ctx, c, peer, m.GetID(), messageOut(m))
			}
		}
	}
	d.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		message(ev.Message, e, u.Message)(ctx)
		return nil
	})
	d.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
		message(ev.Message, e, u.Message)(ctx)
		return nil
	})
	d.OnEditMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateEditMessage) error {
		if visibleEdit(u.Message) {
			message(ev.Edit, e, u.Message)(ctx)
		}
		return nil
	})
	d.OnEditChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateEditChannelMessage) error {
		if visibleEdit(u.Message) {
			message(ev.Edit, e, u.Message)(ctx)
		}
		return nil
	})
	d.OnDeleteMessages(func(ctx context.Context, _ tg.Entities, u *tg.UpdateDeleteMessages) error {
		if ev.Delete != nil {
			ev.Delete(ctx, c, nil, u.Messages)
		}
		return nil
	})
	d.OnDeleteChannelMessages(func(ctx context.Context, _ tg.Entities, u *tg.UpdateDeleteChannelMessages) error {
		if ev.Delete != nil {
			ev.Delete(ctx, c, &tg.PeerChannel{ChannelID: u.ChannelID}, u.Messages)
		}
		return nil
	})
	d.OnTranscribedAudio(func(ctx context.Context, _ tg.Entities, u *tg.UpdateTranscribedAudio) error {
		c.hub.transcribed(u)
		return nil
	})
	gaps := updates.New(updates.Config{Handler: d})
	client, err := newClient(s, gaps, storage)
	if err != nil {
		return err
	}
	c.Client = client
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	up := make(chan struct{})
	var stalled atomic.Bool
	go watchdog(ctx, client, up, func(why string) {
		_ = audit.Log(s.AuditPath(), "service_stalled", "reason", why)
		stalled.Store(true)
		cancel()
	})
	err = client.Run(ctx, func(ctx context.Context) error {
		close(up)
		c.API = client.API()
		st, err := client.Auth().Status(ctx)
		if err != nil {
			return err
		}
		shared.set(c, st.Authorized)
		_ = audit.Log(s.AuditPath(), "service_connected", "authorized", st.Authorized)
		if !st.Authorized {
			shared.mu.Lock()
			warn := !shared.warned
			shared.warned = true
			shared.mu.Unlock()
			if warn && onNoLogin != nil {
				go onNoLogin(ctx)
			}
			// ждём входа через службу (окно управления или tg login)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-shared.loggedIn:
			}
		}
		me, err := c.Self(ctx)
		if err != nil {
			return err
		}
		c.Peers.Save()
		return gaps.Run(ctx, c.API, me.ID, updates.AuthOptions{OnStart: func(ctx context.Context) {
			if onReady != nil {
				go onReady(ctx, c)
			}
		}})
	})
	if stalled.Load() {
		return errStalled
	}
	return err
}

var errStalled = errors.New("соединение с Telegram зависло — клиент пересоздан")

// Сторож соединения. После сна компьютера или смены сети (VPN) gotd бывает
// не может переподключиться сам: запросы часами висят в waitSession, хотя
// свежий клиент подключается сразу. Поэтому соединение проверяется лёгким
// запросом, и зависшее — бросается: служба создаёт клиента заново.
const (
	stallConnect = 2 * time.Minute  // не подключились за это время — заново
	stallEvery   = 30 * time.Second // как часто проверять
	stallPing    = 20 * time.Second // сколько ждать ответа на проверку
	stallMisses  = 3                // столько неудач подряд — зависло
)

func watchdog(ctx context.Context, client *telegram.Client, up <-chan struct{}, stall func(why string)) {
	defer func() {
		// паника внутри gotd не должна ронять службу — считаем соединение зависшим
		if r := recover(); r != nil {
			stall(fmt.Sprintf("сбой проверки соединения: %v", r))
		}
	}()
	select {
	case <-ctx.Done():
		return
	case <-up:
	case <-time.After(stallConnect):
		stall("не подключились за " + stallConnect.String())
		return
	}
	misses := 0
	for {
		sleep(ctx, stallEvery)
		if ctx.Err() != nil {
			return
		}
		// не client.Ping: он пишет в транспорт, не дожидаясь сессии, и посреди
		// переподключения gotd падает на nil (mtproto/write.go). Обычный запрос
		// ждёт сессию — и честно не успевает, если соединение зависло.
		pctx, cancel := context.WithTimeout(ctx, stallPing)
		_, err := client.API().HelpGetNearestDC(pctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			misses = 0
			continue
		}
		if misses++; misses >= stallMisses {
			stall(fmt.Sprintf("%d проверки подряд без ответа: %v", misses, err))
			return
		}
	}
}

func messagePeer(m tg.MessageClass) tg.PeerClass {
	switch v := m.(type) {
	case *tg.Message:
		return v.PeerID
	case *tg.MessageService:
		return v.PeerID
	}
	return nil
}

// visibleEdit — правка, которую видно человеку: Telegram шлёт «правки» и на
// реакции, подгрузку превью ссылок и прочее — у них нет edit_date или стоит
// edit_hide.
func visibleEdit(m tg.MessageClass) bool {
	msg, ok := m.(*tg.Message)
	if !ok || msg.EditHide {
		return false
	}
	date, ok := msg.GetEditDate()
	return ok && date != 0
}

func messageOut(m tg.MessageClass) bool {
	switch v := m.(type) {
	case *tg.Message:
		return v.Out
	case *tg.MessageService:
		return v.Out
	}
	return false
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// ── вход внутри службы ───────────────────────────────────────────────────

type loginState struct {
	phone string
	hash  string
	stage string
}

// LoginStep — где сейчас вход: need_code, need_password, done.
type LoginStep struct {
	Stage string `json:"stage"`
	User  string `json:"user,omitempty"`
}

// ServiceLoginBegin — отправить код на телефон (вход через службу).
func ServiceLoginBegin(ctx context.Context, phone string) (LoginStep, error) {
	c, authorized, err := shared.wait(ctx, 30*time.Second)
	if err != nil {
		return LoginStep{}, err
	}
	if authorized {
		return loginDone(ctx, c)
	}
	sent, err := c.Client.Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
	if err != nil {
		return LoginStep{}, err
	}
	code, ok := sent.(*tg.AuthSentCode)
	if !ok {
		return LoginStep{}, errors.New("Telegram не прислал код (неожиданный ответ)")
	}
	shared.mu.Lock()
	shared.login = &loginState{phone: phone, hash: code.PhoneCodeHash, stage: "need_code"}
	shared.mu.Unlock()
	return LoginStep{Stage: "need_code"}, nil
}

// ServiceLoginAnswer — код из Telegram или пароль 2FA, смотря что ждём.
func ServiceLoginAnswer(ctx context.Context, value string) (LoginStep, error) {
	c, _, err := shared.wait(ctx, 30*time.Second)
	if err != nil {
		return LoginStep{}, err
	}
	shared.mu.Lock()
	st := shared.login
	shared.mu.Unlock()
	if st == nil {
		return LoginStep{}, errors.New("вход не начат")
	}
	value = strings.TrimSpace(value)
	switch st.stage {
	case "need_code":
		_, err := c.Client.Auth().SignIn(ctx, st.phone, value, st.hash)
		if errors.Is(err, auth.ErrPasswordAuthNeeded) {
			st.stage = "need_password"
			return LoginStep{Stage: "need_password"}, nil
		}
		if err != nil {
			return LoginStep{}, err
		}
	case "need_password":
		if _, err := c.Client.Auth().Password(ctx, value); err != nil {
			return LoginStep{}, err
		}
	default:
		return LoginStep{}, errors.New("вход не начат")
	}
	shared.mu.Lock()
	shared.login = nil
	shared.authorized = true
	shared.keyKept, shared.warned = false, false
	shared.mu.Unlock()
	select {
	case shared.loggedIn <- struct{}{}:
	default:
	}
	return loginDone(ctx, c)
}

func loginDone(ctx context.Context, c *Conn) (LoginStep, error) {
	me, err := c.Self(ctx)
	if err != nil {
		return LoginStep{}, err
	}
	name := strings.TrimSpace(me.FirstName + " " + me.LastName)
	if me.Username != "" {
		name += " @" + me.Username
	}
	return LoginStep{Stage: "done", User: name + " (id " + strconv.FormatInt(me.ID, 10) + ")"}, nil
}

// ServiceLogout — отозвать сессию; служба остаётся ждать нового входа.
func ServiceLogout(ctx context.Context) error {
	c, _, err := shared.wait(ctx, 30*time.Second)
	if err != nil {
		return err
	}
	if _, err := c.API.AuthLogOut(ctx); err != nil {
		return err
	}
	shared.mu.Lock()
	shared.authorized = false
	shared.reset = true
	shared.warned = true // вышли сами — предупреждать не о чем
	cancel := shared.cancel
	shared.mu.Unlock()
	// соединение с отозванным ключом бесполезно — переподключимся с чистого листа
	if cancel != nil {
		cancel()
	}
	return nil
}

// ServiceAuthorized — есть ли вход (для статуса службы).
func ServiceAuthorized() (connected, authorized bool) {
	shared.mu.Lock()
	defer shared.mu.Unlock()
	return shared.conn != nil, shared.authorized
}
