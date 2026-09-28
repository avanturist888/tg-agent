package tgc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/session"

	"tgagent/internal/audit"
	"tgagent/internal/config"
)

// Защита ключа сессии.
//
// Получив от сервера транспортную ошибку -404 («ключ не найден»), gotd прямо
// в открытом соединении создаёт новый постоянный ключ, а при следующем
// переподключении записывает его поверх старого. Новый ключ не авторизован:
// одна такая ошибка (после сна, при смене сети или VPN) стоила входа в
// аккаунт — старый, рабочий ключ был затёрт.
//
// Поэтому ключ, который уже лежит в файле, gotd заменить не может: запись
// отклоняется, а клиент перезапускается со старым ключом из файла. Если
// сервер и правда его забыл, через KeyGrace попыток новый ключ всё-таки
// принимается (старый файл остаётся рядом, *.lost-*) — нужен новый вход.

// KeyGrace — сколько отклонять подмену ключа, прежде чем поверить серверу.
const KeyGrace = 10 * time.Minute

var errKeyKept = errors.New("Telegram не узнал ключ сессии; оставляю прежний ключ и переподключаюсь")

// KeyRejected — gotd пытался заменить ключ сессии новым.
type KeyRejected struct{}

func (KeyRejected) Error() string {
	return "Telegram на мгновение не узнал ключ сессии — служба переподключается с прежним ключом, повтори чуть позже."
}

type guardedStorage struct {
	file     session.FileStorage
	s        *config.Settings
	login    bool   // вход: новый ключ — это нормально
	rejected func() // подмена отклонена — клиент надо перезапустить
}

func newGuardedStorage(s *config.Settings, login bool, rejected func()) *guardedStorage {
	return &guardedStorage{file: session.FileStorage{Path: s.SessionPath}, s: s, login: login, rejected: rejected}
}

func (g *guardedStorage) LoadSession(ctx context.Context) ([]byte, error) {
	return g.file.LoadSession(ctx)
}

func (g *guardedStorage) StoreSession(ctx context.Context, data []byte) error {
	old, err := g.file.LoadSession(ctx)
	if err != nil || len(old) == 0 {
		return g.file.StoreSession(ctx, data)
	}
	was, now := authKeyID(old), authKeyID(data)
	if len(was) == 0 || bytes.Equal(was, now) {
		_ = os.Remove(g.lossPath()) // прежний ключ снова в деле
		return g.file.StoreSession(ctx, data)
	}
	if !g.login {
		since := g.lossSince()
		if since.IsZero() {
			since = time.Now()
			_ = os.WriteFile(g.lossPath(), []byte(strconv.FormatInt(since.Unix(), 10)), 0o600)
		}
		if time.Since(since) < KeyGrace {
			_ = audit.Log(g.s.AuditPath(), "auth_key_kept", "since", since.Format(time.RFC3339))
			if g.rejected != nil {
				g.rejected()
			}
			return errKeyKept
		}
	}
	// сдаёмся (или это вход): прежний ключ — рядом, на случай разбирательства
	lost := g.s.SessionPath + ".lost-" + time.Now().Format("0102-150405")
	_ = os.WriteFile(lost, old, 0o600)
	_ = os.Remove(g.lossPath())
	_ = audit.Log(g.s.AuditPath(), "auth_key_replaced", "login", g.login, "old", lost)
	return g.file.StoreSession(ctx, data)
}

func (g *guardedStorage) lossPath() string { return g.s.SessionPath + ".keyloss" }

func (g *guardedStorage) lossSince() time.Time {
	raw, err := os.ReadFile(g.lossPath())
	if err != nil {
		return time.Time{}
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// authKeyID — id ключа из файла сессии gotd ({"Version":1,"Data":{...}}).
func authKeyID(raw []byte) []byte {
	var v struct {
		Data struct {
			AuthKeyID []byte
		}
	}
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v.Data.AuthKeyID
}
