// Package inbox — уведомление в сессию Claude Code через её входящий канал.
//
// У каждой сессии Claude Code (и у той, что запускает T3 Code через Agent
// SDK) есть входящий канал для сообщений от других сессий: named pipe на
// Windows, unix-сокет на остальных системах. Адрес и токен сессия отдаёт
// своим дочерним процессам в CLAUDE_CODE_MESSAGING_SOCKET и
// CLAUDE_CODE_MESSAGING_TOKEN. Сообщение с этим токеном Claude Code считает
// пришедшим от своего же потомка и доставляет всегда; если сессия простаивает,
// оно начинает новый ход. См.
// https://code.claude.com/docs/en/cross-session-messaging («The session's inbox socket»).
//
// Протокол: строки JSON. Первая — {"type":"auth","token":...} (на Windows
// обязательна), дальше — сообщения. Строку надо прислать в течение 30 с после
// подключения, поэтому соединение открываем, когда текст уже готов.
package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"time"
)

// ErrNoSocket — у сессии нет входящего канала (тред Codex).
var ErrNoSocket = errors.New("inbox: у сессии нет входящего канала")

// Addr — входящий канал сессии Claude Code.
type Addr struct {
	Session string `json:"session,omitempty"` // CLAUDE_CODE_SESSION_ID: не меняется при возобновлении
	Socket  string `json:"socket"`
	Token   string `json:"token"`
	// Kind — "codex": тред Codex в T3. Входящего канала у него нет (Codex —
	// один общий процесс на все треды, переменных сессии у его MCP нет), а
	// Session — номер его треда из _meta вызова; будят его только через T3.
	Kind string `json:"kind,omitempty"`
}

// Codex — тред Codex: будить только через T3 Code.
func (a Addr) Codex() bool { return a.Kind == KindCodex }

const KindCodex = "codex"

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// FromMeta — тред Codex из _meta вызова инструмента: Codex кладёт туда свой
// номер треда (threadId, thread_id, в том числе внутри JSON-строки
// x-codex-turn-metadata). nil — не Codex.
func FromMeta(meta map[string]any) *Addr {
	if len(meta) == 0 {
		return nil
	}
	var found string
	var walk func(any)
	walk = func(v any) {
		if found != "" {
			return
		}
		switch x := v.(type) {
		case map[string]any:
			for _, k := range []string{"threadId", "thread_id"} {
				if s, ok := x[k].(string); ok && uuidRe.MatchString(s) {
					found = s
					return
				}
			}
			for _, val := range x {
				walk(val)
			}
		case string:
			if t := strings.TrimSpace(x); strings.HasPrefix(t, "{") {
				var m any
				if json.Unmarshal([]byte(t), &m) == nil {
					walk(m)
				}
			}
		}
	}
	walk(meta)
	if found == "" {
		return nil
	}
	return &Addr{Session: found, Kind: KindCodex}
}

// FromEnv — канал сессии, потомком которой запущен этот процесс (nil — нет).
func FromEnv() *Addr {
	a := &Addr{
		Session: os.Getenv("CLAUDE_CODE_SESSION_ID"),
		Socket:  os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"),
		Token:   os.Getenv("CLAUDE_CODE_MESSAGING_TOKEN"),
	}
	if a.Socket == "" || a.Token == "" {
		return nil
	}
	return a
}

// Key — чья это сессия: id сессии, а у старых Claude Code без него — сам канал.
func (a Addr) Key() string {
	if a.Session != "" {
		return a.Session
	}
	return "socket:" + a.Socket
}

// Send — доставить сообщение в сессию. from — подпись отправителя.
func Send(ctx context.Context, a Addr, from, text string) error {
	if a.Socket == "" {
		return ErrNoSocket
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := dial(ctx, a.Socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	var buf []byte
	for _, v := range []any{
		map[string]string{"type": "auth", "token": a.Token},
		map[string]any{"type": "user", "from": from, "priority": "next",
			"message": map[string]string{"role": "user", "content": text}},
	} {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		buf = append(append(buf, raw...), '\n')
	}
	_, err = conn.Write(buf)
	return err
}
