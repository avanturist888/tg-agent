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
	"os"
	"time"
)

// Addr — входящий канал сессии Claude Code.
type Addr struct {
	Session string `json:"session,omitempty"` // CLAUDE_CODE_SESSION_ID: не меняется при возобновлении
	Socket  string `json:"socket"`
	Token   string `json:"token"`
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
