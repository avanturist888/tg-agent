package tgc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"

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
	return fn(ctx, c)
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

// OnMessage — новое сообщение в любом чате аккаунта (peer, id сообщения).
type OnMessage func(ctx context.Context, c *Conn, peer tg.PeerClass, msgID int)

// Serve — держать постоянное соединение, пока не отменят ctx. Переподключается
// сам. onMessage получает каждое новое сообщение; onReady вызывается после
// каждого (пере)подключения с действующим входом — чтобы догнать пропущенное.
func Serve(ctx context.Context, s *config.Settings, onMessage OnMessage, onReady func(ctx context.Context, c *Conn)) error {
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
		}
		started := time.Now()
		octx, cancel := context.WithCancel(ctx)
		shared.mu.Lock()
		shared.cancel = cancel
		shared.mu.Unlock()
		err := serveOnce(octx, s, onMessage, onReady)
		cancel()
		shared.set(nil, false)
		if ctx.Err() != nil {
			return nil
		}
		if reset || errors.Is(err, context.Canceled) {
			continue // переподключение по нашей же просьбе
		}
		_ = audit.Log(s.AuditPath(), "service_disconnected", "error", fmt.Sprint(err))
		if time.Since(started) > 5*time.Minute {
			backoff = 2 * time.Second // долго работало — сбой разовый
		}
		sleep(ctx, backoff)
		backoff = min(backoff*2, time.Minute)
	}
	return nil
}

func serveOnce(ctx context.Context, s *config.Settings, onMessage OnMessage, onReady func(ctx context.Context, c *Conn)) error {
	c := &Conn{Settings: s, hub: newUpdateHub(), Peers: LoadPeers(s.PeersPath())}
	d := tg.NewUpdateDispatcher()
	d.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		c.Peers.RememberEntities(e)
		if peer := messagePeer(u.Message); peer != nil {
			onMessage(ctx, c, peer, u.Message.GetID())
		}
		return nil
	})
	d.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
		c.Peers.RememberEntities(e)
		if peer := messagePeer(u.Message); peer != nil {
			onMessage(ctx, c, peer, u.Message.GetID())
		}
		return nil
	})
	d.OnTranscribedAudio(func(ctx context.Context, _ tg.Entities, u *tg.UpdateTranscribedAudio) error {
		c.hub.transcribed(u)
		return nil
	})
	gaps := updates.New(updates.Config{Handler: d})
	client, err := newClient(s, gaps)
	if err != nil {
		return err
	}
	c.Client = client
	return client.Run(ctx, func(ctx context.Context) error {
		c.API = client.API()
		st, err := client.Auth().Status(ctx)
		if err != nil {
			return err
		}
		shared.set(c, st.Authorized)
		_ = audit.Log(s.AuditPath(), "service_connected", "authorized", st.Authorized)
		if !st.Authorized {
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
