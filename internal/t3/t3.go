// Package t3 — агенты, которые работают в T3 Code.
//
// Два пути.
//
// Местный десктоп (Wake). T3 Code останавливает процесс Claude у треда,
// простоявшего 30 минут без фоновой работы (ProviderSessionReaper), — вместе
// с процессом пропадает входящий канал сессии, и разбудить агента через inbox
// нельзя. Тогда служба просит сам T3 начать в треде ход (thread.turn.start):
// T3 возобновит сессию, а уведомление появится в треде обычным сообщением.
// Тред ищется по id сессии Claude в базе десктопа (state.sqlite).
//
// Окружения из config/t3.toml (Env, env.go) — контейнеры со своим сервером
// T3. Их базу не прочитать, а курсор сессии Claude API наружу не отдаёт,
// поэтому там служба заводит свои треды (thread.create) и будит их по id.
//
// Всё здесь необязательное. Нет T3 Code, его сервер не отвечает или сменил
// протокол — Wake возвращает Unavailable, и остальное работает как без него.
// API и база T3 внутренние (alpha): после его обновления путь может сломаться,
// подписки через inbox от этого не страдают.
//
// Доступ: одноразовый pairing-код (у десктопа — встроенная команда T3 `auth
// pairing create`) меняется на токен с правами только на треды
// (orchestration:read, orchestration:operate). Токен живёт 30 дней и лежит в
// data/t3-token.json (у окружений — data/t3-token-<имя>.json); в списке
// сессий T3 он подписан tg-agent.
package t3

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Unavailable — T3 Code не установлен, не запущен или его API не тот.
var Unavailable = errors.New("T3 Code не найден или не отвечает")

// NoThread — сессия не из T3 (или тред удалён/в архиве).
var NoThread = errors.New("у сессии нет треда в T3 Code")

const (
	protocolVersion = 1 // orchestrationProtocolVersion, под который написан этот код
	tokenScopes     = "orchestration:read orchestration:operate"
	tokenLabel      = "tg-agent"
	renewBefore     = 3 * 24 * time.Hour
)

type install struct {
	home string // ~/.t3 (или T3CODE_HOME)
	env  *Env   // его сервер: http://127.0.0.1:<порт>
}

var client = &http.Client{Timeout: 15 * time.Second}

func home() string {
	if h := os.Getenv("T3CODE_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".t3")
}

// find — запущенный T3 Code с понятным нам API.
func find(ctx context.Context) (*install, error) {
	in := &install{home: home()}
	raw, err := os.ReadFile(filepath.Join(in.home, "userdata", "server-runtime.json"))
	if err != nil {
		return nil, Unavailable
	}
	var rt struct {
		Port int `json:"port"`
	}
	if json.Unmarshal(raw, &rt) != nil || rt.Port == 0 {
		return nil, Unavailable
	}
	in.env = &Env{Name: "local", Origin: fmt.Sprintf("http://127.0.0.1:%d", rt.Port), pairing: in.pairingCode,
		revoke: func(ctx context.Context, sid string) {
			_, _ = in.cli(ctx, "auth", "session", "revoke", "--base-dir", in.home, sid)
		}}
	if _, _, err := in.env.Descriptor(ctx); err != nil {
		return nil, err
	}
	return in, nil
}

// threadFor — тред T3, в котором живёт сессия Claude Code.
func (in *install) threadFor(ctx context.Context, session string) (string, error) {
	path := filepath.Join(in.home, "userdata", "state.sqlite")
	if _, err := os.Stat(path); err != nil {
		return "", Unavailable
	}
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=busy_timeout(3000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var thread string
	err = db.QueryRowContext(ctx, `SELECT thread_id FROM provider_session_runtime
		WHERE json_extract(resume_cursor_json, '$.resume') = ? ORDER BY last_seen_at DESC LIMIT 1`, session).Scan(&thread)
	if errors.Is(err, sql.ErrNoRows) {
		return "", NoThread
	}
	if err != nil {
		return "", fmt.Errorf("база T3: %w", err)
	}
	return thread, nil
}

// Wake — начать в треде сессии ход с текстом text.
func Wake(ctx context.Context, tokenPath, session, text string) error {
	in, err := find(ctx)
	if err != nil {
		return err
	}
	thread, err := in.threadFor(ctx, session)
	if err != nil {
		return err
	}
	in.env.TokenPath = tokenPath
	// процесс сессии уже остановлен, так что «идущий» ход в снимке — старый
	_, err = in.env.startTurn(ctx, thread, text, true)
	if errors.Is(err, ThreadGone) {
		return NoThread // тред удалён или в архиве — владелец его закрыл, не тревожим
	}
	return err
}

// pairingCode — одноразовый код от встроенной команды T3 (живёт 5 минут).
func (in *install) pairingCode(ctx context.Context) (string, error) {
	out, err := in.cli(ctx, "auth", "pairing", "create", "--base-dir", in.home, "--ttl", "5m", "--label", tokenLabel, "--json")
	if err != nil {
		return "", err
	}
	code := credential(out)
	if code == "" {
		return "", errors.New("команда T3 не выдала код доступа")
	}
	return code, nil
}

// cli — команда сервера T3 (тот же exe в режиме node).
func (in *install) cli(ctx context.Context, args ...string) ([]byte, error) {
	exe, bin, err := locateCLI()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, append([]string{bin}, args...)...)
	cmd.Env = append(os.Environ(), "ELECTRON_RUN_AS_NODE=1")
	hideWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("команда T3 %s: %v %s", strings.Join(args[:2], " "), err, tail(stderr.String(), 300))
	}
	return out, nil
}

// locateCLI — exe T3 Code и скрипт его сервера.
func locateCLI() (exe, bin string, err error) {
	dirs := []string{os.Getenv("T3CODE_APP_DIR")}
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		dirs = append(dirs, filepath.Join(la, "Programs", "t3code"))
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		bin = filepath.Join(dir, "resources", "server.asar", "apps", "server", "dist", "bin.mjs")
		if _, err := os.Stat(filepath.Join(dir, "resources", "server.asar")); err != nil {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(dir, "T3 Code*.exe"))
		for _, m := range matches {
			if !strings.Contains(strings.ToLower(filepath.Base(m)), "uninstall") {
				return m, bin, nil
			}
		}
	}
	return "", "", fmt.Errorf("%w: не найден exe T3 Code", Unavailable)
}

// tokenSession — id сессии T3 из токена (полезная нагрузка до точки).
func tokenSession(tok string) string {
	payload, _, ok := strings.Cut(tok, ".")
	if !ok {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return ""
	}
	var claims struct {
		Sid string `json:"sid"`
	}
	_ = json.Unmarshal(raw, &claims)
	return claims.Sid
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40 // uuid v4
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
