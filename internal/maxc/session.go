package maxc

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Session — вход в MAX, сохранённый на диске (data/max/session.json, 0600).
// Токен даёт полный доступ к аккаунту: не печатать, не писать в аудит.
type Session struct {
	Token     string    `json:"token"`
	DeviceID  string    `json:"device_id"`
	UserAgent UserAgent `json:"user_agent"`
	UserID    int64     `json:"user_id,omitempty"`
	Created   time.Time `json:"created"`
	Source    string    `json:"source"` // qr или pymax (перенесена из PyMax)
}

// ErrNoSession — входа в MAX ещё не было.
var ErrNoSession = errors.New("maxc: входа в MAX нет — tg max login")

// NewDeviceID — id устройства, как у PyMax (16 hex).
func NewDeviceID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// LoadSession — сессия из файла; нет файла — ErrNoSession.
func LoadSession(path string) (*Session, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.Token == "" || s.DeviceID == "" {
		return nil, ErrNoSession
	}
	return &s, nil
}

// Save — записать атомарно, только для владельца.
func (s *Session) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ImportPyMax — сессия веб-клиента PyMax (SQLite, таблица sessions): тот же
// токен, id устройства и user-agent — сервер видит прежнее устройство.
func ImportPyMax(ctx context.Context, dbPath string) (*Session, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var token, device string
	var ua sql.NullString
	err = db.QueryRowContext(ctx, `SELECT token, device_id, user_agent FROM sessions LIMIT 1`).Scan(&token, &device, &ua)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%s: сессий нет", dbPath)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dbPath, err)
	}
	s := &Session{Token: token, DeviceID: device, Created: time.Now(), Source: "pymax"}
	if ua.Valid && ua.String != "" {
		if err := json.Unmarshal([]byte(ua.String), &s.UserAgent); err != nil {
			return nil, fmt.Errorf("%s: user_agent: %w", dbPath, err)
		}
	}
	if s.UserAgent.DeviceType == "" {
		s.UserAgent = WebUserAgent("")
	}
	if s.UserAgent.DeviceType != "WEB" {
		return nil, fmt.Errorf("%s: сессия не веб-клиента (%s) — её вход шёл по-другому", dbPath, s.UserAgent.DeviceType)
	}
	return s, nil
}

// Open — подключиться и войти по сохранённой сессии.
func Open(ctx context.Context, s *Session, opts Options) (*Conn, *LoginResult, error) {
	if opts.UserAgent == "" {
		opts.UserAgent = s.UserAgent.HeaderUserAgent
	}
	c, err := Dial(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	if err := c.Handshake(ctx, s.UserAgent, s.DeviceID); err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("maxc: рукопожатие: %w", err)
	}
	r, err := c.Login(ctx, s.Token)
	if err != nil {
		c.Close()
		return nil, nil, err
	}
	return c, r, nil
}
