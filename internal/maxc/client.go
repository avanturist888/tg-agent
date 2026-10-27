package maxc

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Client — постоянное подключение к MAX по сохранённой сессии: входит,
// после обрыва переподключается сам (пауза растёт до минуты). Отозванный
// токен — Run возвращает ErrTokenRevoked: новый вход — только владелец.
type Client struct {
	Session *Session
	Options Options // OnEvent здесь не нужен — см. OnEvent ниже
	// OnEvent — разобранные события; из одной горутины, по порядку.
	OnEvent func(Event)
	// OnLogin — после каждого входа (и после переподключения): здесь
	// сверять пропущенное за время обрыва (LoginResult.Chats — последние
	// сообщения чатов).
	OnLogin func(*LoginResult)
	// OnDisconnect — соединение порвалось (до новой попытки).
	OnDisconnect func(error)

	mu    sync.Mutex
	conn  *Conn
	ready chan struct{} // закрыт, пока conn есть
}

var (
	backoffMin = time.Second // тесты укорачивают
	backoffMax = time.Minute
)

const stableFor = time.Minute // столько прожило — паузу сбрасываем

// Run — держать подключение до отмены ctx или отзыва токена.
func (cl *Client) Run(ctx context.Context) error {
	cl.mu.Lock()
	if cl.ready == nil {
		cl.ready = make(chan struct{})
	}
	cl.mu.Unlock()
	opts := cl.Options
	opts.OnEvent = func(f Frame) {
		if cl.OnEvent == nil {
			return
		}
		ev, err := ParseEvent(f)
		if err != nil {
			ev = Event{Kind: EventOther, Frame: f} // кадр есть, разобрать не вышло
		}
		cl.OnEvent(ev)
	}
	pause := backoffMin
	for {
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		c, r, err := Open(cctx, cl.Session, opts)
		cancel()
		if errors.Is(err, ErrTokenRevoked) {
			return err
		}
		if err == nil {
			started := time.Now()
			cl.setConn(c)
			if cl.OnLogin != nil {
				cl.OnLogin(r)
			}
			select {
			case <-ctx.Done():
				cl.setConn(nil)
				c.Close()
				return ctx.Err()
			case <-c.Done():
			}
			cl.setConn(nil)
			err = c.Err()
			if time.Since(started) > stableFor {
				pause = backoffMin
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cl.OnDisconnect != nil {
			cl.OnDisconnect(err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pause):
		}
		pause = min(pause*2, backoffMax)
	}
}

func (cl *Client) setConn(c *Conn) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if c != nil {
		cl.conn = c
		select {
		case <-cl.ready:
		default:
			close(cl.ready)
		}
		return
	}
	cl.conn = nil
	select {
	case <-cl.ready:
		cl.ready = make(chan struct{})
	default:
	}
}

// Conn — текущее соединение; если его нет — ждать его не дольше ctx.
func (cl *Client) Conn(ctx context.Context) (*Conn, error) {
	for {
		cl.mu.Lock()
		if cl.ready == nil {
			cl.ready = make(chan struct{})
		}
		c, ready := cl.conn, cl.ready
		cl.mu.Unlock()
		if c != nil {
			return c, nil
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("maxc: нет связи с MAX")
		case <-ready:
		}
	}
}
