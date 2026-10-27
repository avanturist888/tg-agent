package maxc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Опкоды, которыми пользуемся (полный список — pymax/protocol/enums.py).
const (
	OpPing           uint16 = 1
	OpSessionInit    uint16 = 6
	OpLogin          uint16 = 19
	OpLogout         uint16 = 20
	OpContactInfo    uint16 = 32
	OpChatHistory    uint16 = 49
	OpMsgSend        uint16 = 64
	OpMsgDelete      uint16 = 66
	OpMsgEdit        uint16 = 67
	OpMsgGet         uint16 = 71
	OpCheckPassword  uint16 = 115
	OpNotifMessage   uint16 = 128
	OpNotifMark      uint16 = 130
	OpNotifPresence  uint16 = 132
	OpNotifMsgDelete uint16 = 142
	OpGetQR          uint16 = 288
	OpQRStatus       uint16 = 289
	OpLoginByQR      uint16 = 291
)

const (
	DefaultURL    = "wss://api.oneme.ru/websocket"
	DefaultOrigin = "https://web.max.ru"
	pingEvery     = 30 * time.Second
	callTimeout   = 30 * time.Second
	readLimit     = 16 << 20
)

// ErrClosed — соединение закрыто (нами или сервером).
var ErrClosed = errors.New("maxc: соединение закрыто")

// Error — отказ сервера (кадр cmd=3).
type Error struct {
	Opcode    uint16
	Code      string `json:"error"`
	Message   string `json:"message"`
	Localized string `json:"localizedMessage"`
	Title     string `json:"title"`
}

func (e *Error) Error() string {
	msg := e.Localized
	if msg == "" {
		msg = e.Message
	}
	return fmt.Sprintf("MAX opcode %d: %s [%s]", e.Opcode, msg, e.Code)
}

// Options — настройки соединения; нули — значения по умолчанию.
type Options struct {
	URL, Origin string
	UserAgent   string // заголовок User-Agent рукопожатия WebSocket
	HTTPClient  *http.Client
	// OnEvent — кадры, которые сервер прислал сам (новые сообщения, правки,
	// прочтения…). Зовётся из одной горутины в порядке прихода; долгая
	// работа в нём задерживает следующие события, но не ответы на запросы.
	OnEvent func(Frame)
	// Interactive — пинг с interactive=true: сервер считает, что человек у
	// экрана (так шлёт PyMax). false — просто держим соединение.
	Interactive bool
}

// Conn — одно соединение WebSocket с сервером MAX.
type Conn struct {
	ws   *websocket.Conn
	opts Options

	wmu sync.Mutex // писать в WebSocket — по одному

	mu      sync.Mutex
	seq     uint16
	pending map[uint16]chan Frame
	err     error // почему закрыто

	evMu   sync.Mutex
	evCond *sync.Cond
	events []Frame

	done   chan struct{}
	cancel context.CancelFunc
}

// Dial — открыть соединение. Входа в нём ещё нет: дальше Handshake и Login.
func Dial(ctx context.Context, opts Options) (*Conn, error) {
	if opts.URL == "" {
		opts.URL = DefaultURL
	}
	if opts.Origin == "" {
		opts.Origin = DefaultOrigin
	}
	h := http.Header{"Origin": {opts.Origin}}
	if opts.UserAgent != "" {
		h.Set("User-Agent", opts.UserAgent)
	}
	ws, _, err := websocket.Dial(ctx, opts.URL, &websocket.DialOptions{HTTPHeader: h, HTTPClient: opts.HTTPClient})
	if err != nil {
		return nil, fmt.Errorf("maxc: подключение к %s: %w", opts.URL, err)
	}
	ws.SetReadLimit(readLimit)
	lctx, cancel := context.WithCancel(context.Background())
	c := &Conn{ws: ws, opts: opts, pending: map[uint16]chan Frame{}, done: make(chan struct{}), cancel: cancel}
	c.evCond = sync.NewCond(&c.evMu)
	go c.readLoop(lctx)
	go c.dispatch()
	go c.pingLoop(lctx)
	return c, nil
}

// Done — закрывается вместе с соединением; причина — Err.
func (c *Conn) Done() <-chan struct{} { return c.done }

func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close — закрыть соединение; ждущие ответа получат ErrClosed.
func (c *Conn) Close() error {
	c.fail(ErrClosed)
	return nil
}

func (c *Conn) fail(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	for seq, ch := range c.pending {
		close(ch)
		delete(c.pending, seq)
	}
	c.mu.Unlock()
	c.cancel()
	_ = c.ws.Close(websocket.StatusNormalClosure, "")
	c.evMu.Lock()
	c.evCond.Broadcast()
	c.evMu.Unlock()
	close(c.done)
}

// Invoke — запрос и ответ на него. out — указатель на структуру с тегами
// json (или nil). Отказ сервера — *Error.
func (c *Conn) Invoke(ctx context.Context, opcode uint16, payload, out any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, callTimeout)
		defer cancel()
	}
	if payload == nil {
		payload = map[string]any{}
	}
	ch := make(chan Frame, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	seq := c.nextSeq()
	c.pending[seq] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, seq)
		c.mu.Unlock()
	}()

	data, err := encodeFrame(CmdRequest, seq, opcode, payload)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	err = c.ws.Write(ctx, websocket.MessageBinary, data)
	c.wmu.Unlock()
	if err != nil {
		c.fail(fmt.Errorf("maxc: запись: %w", err))
		return c.Err()
	}
	select {
	case f, ok := <-ch:
		if !ok {
			return c.Err()
		}
		if f.Cmd == CmdError {
			e := &Error{Opcode: opcode}
			_ = f.Decode(e)
			return e
		}
		if out != nil {
			if err := f.Decode(out); err != nil {
				return fmt.Errorf("maxc: ответ на opcode %d: %w", opcode, err)
			}
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("maxc: нет ответа на opcode %d: %w", opcode, ctx.Err())
	}
}

// nextSeq — следующий свободный номер запроса (под c.mu).
func (c *Conn) nextSeq() uint16 {
	for {
		c.seq++
		if _, busy := c.pending[c.seq]; !busy {
			return c.seq
		}
	}
}

func (c *Conn) readLoop(ctx context.Context) {
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			c.fail(fmt.Errorf("maxc: чтение: %w", err))
			return
		}
		if typ != websocket.MessageBinary {
			continue // протокол JSON (v11) не используем
		}
		frames, err := decodeFrames(data)
		for _, f := range frames {
			c.route(f)
		}
		if err != nil {
			c.fail(err)
			return
		}
	}
}

func (c *Conn) route(f Frame) {
	if f.Cmd == CmdResponse || f.Cmd == CmdError {
		c.mu.Lock()
		ch := c.pending[f.Seq]
		delete(c.pending, f.Seq)
		c.mu.Unlock()
		if ch != nil {
			ch <- f
			return
		}
	}
	c.evMu.Lock()
	c.events = append(c.events, f)
	c.evCond.Signal()
	c.evMu.Unlock()
}

// dispatch — события по одному в порядке прихода.
func (c *Conn) dispatch() {
	for {
		c.evMu.Lock()
		for len(c.events) == 0 && c.Err() == nil {
			c.evCond.Wait()
		}
		if len(c.events) == 0 {
			c.evMu.Unlock()
			return
		}
		f := c.events[0]
		c.events = c.events[1:]
		c.evMu.Unlock()
		if c.opts.OnEvent != nil {
			c.opts.OnEvent(f)
		}
	}
}

func (c *Conn) pingLoop(ctx context.Context) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, callTimeout)
		err := c.Invoke(pctx, OpPing, map[string]any{"interactive": c.opts.Interactive}, nil)
		cancel()
		if err != nil && ctx.Err() == nil {
			c.fail(fmt.Errorf("maxc: пинг: %w", err))
			return
		}
	}
}
