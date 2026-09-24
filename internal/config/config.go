// Package config читает .env и белый список чатов config/chats.toml.
//
// chats.toml и data/.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/joho/godotenv"

	"tgagent/internal/omap"
)

// Error — проблема в .env или chats.toml; текст адресован человеку.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func cfgErr(format string, a ...any) error { return &Error{Msg: fmt.Sprintf(format, a...)} }

// Denied — чат вне белого списка или нет прав.
type Denied struct{ Msg string }

func (e *Denied) Error() string { return e.Msg }

// Root — корень установки tg-agent (там, где config/ и data/).
var Root = findRoot()

func findRoot() string {
	if home := os.Getenv("TG_AGENT_HOME"); home != "" {
		abs, _ := filepath.Abs(home)
		return abs
	}
	// бинарник лежит где-то внутри установки (bin/tg.exe) —
	// поднимаемся, пока не встретим config/ рядом с AGENTS.md
	starts := []string{}
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		starts = append(starts, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	for _, start := range starts {
		dir := start
		for {
			if isRoot(dir) {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if len(starts) > 0 {
		return starts[0]
	}
	return "."
}

func isRoot(dir string) bool {
	if st, err := os.Stat(filepath.Join(dir, "config")); err != nil || !st.IsDir() {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "AGENTS.md"))
	return err == nil
}

// ChatRule — запись белого списка.
type ChatRule struct {
	Alias string
	Peer  any // int64 или string ("me", "@username")
	Title string
	Read  bool
	Send  bool
	Auto  bool // auto_send = true: без кнопки, но с окном на отмену
	Note  string
}

// PeerString — peer как строка (id числом, "me" или "@username").
func (r ChatRule) PeerString() string {
	switch p := r.Peer.(type) {
	case int64:
		return strconv.FormatInt(p, 10)
	case string:
		return p
	}
	return fmt.Sprint(r.Peer)
}

// AsMap — описание чата для агента (tg_list_chats).
func (r ChatRule) AsMap() *omap.Map {
	return omap.New().
		Set("alias", r.Alias).
		Set("title", r.Title).
		Set("can_read", r.Read).
		Set("can_send", r.Send).
		Set("auto_send", r.AutoSend()).
		Set("note", r.Note)
}

// AutoSend — отправка без кнопки, с окном отмены (auto_send = true вместе с send = true).
func (r ChatRule) AutoSend() bool { return r.Send && r.Auto }

// Settings — всё, что прочитано из .env и chats.toml.
type Settings struct {
	APIID              int
	APIHash            string
	Phone              string
	SessionPath        string
	SendPolicy         string
	DraftTTLMin        int
	Proxy              *url.URL
	MaxLimit           int
	BotToken           string
	ApprovalChatID     int64
	BotProxy           string
	ApprovalTimeoutSec int
	TranscribePerCall  int
	MaxFileMB          int
	MaxDownloadMB      int
	Reactions          bool
	AutoSendDelaySec   int
	Chats              map[string]ChatRule
	ChatOrder          []string // порядок из chats.toml
}

func (s *Settings) DataDir() string         { return filepath.Join(Root, "data") }
func (s *Settings) BotReady() bool          { return s.BotToken != "" && s.ApprovalChatID != 0 }
func (s *Settings) UpdatesLockPath() string { return filepath.Join(s.DataDir(), "bot-updates") }
func (s *Settings) DownloadsDir() string    { return filepath.Join(s.DataDir(), "downloads") }
func (s *Settings) TranscriptsPath() string { return filepath.Join(s.DataDir(), "transcripts.json") }
func (s *Settings) OffsetPath() string      { return filepath.Join(s.DataDir(), "bot_offset.json") }
func (s *Settings) OutboxPath() string      { return filepath.Join(s.DataDir(), "outbox.json") }
func (s *Settings) AuditPath() string       { return filepath.Join(s.DataDir(), "audit.jsonl") }
func (s *Settings) PeersPath() string       { return filepath.Join(s.DataDir(), "peers.json") }
func (s *Settings) OutboxFilesDir() string  { return filepath.Join(s.DataDir(), "outbox_files") }
func (s *Settings) FeedsDir() string        { return filepath.Join(s.DataDir(), "feeds") }
func (s *Settings) ChatsPath() string       { return filepath.Join(Root, "config", "chats.toml") }

// Rules — правила в порядке из chats.toml.
func (s *Settings) Rules() []ChatRule {
	out := make([]ChatRule, 0, len(s.ChatOrder))
	for _, a := range s.ChatOrder {
		out = append(out, s.Chats[a])
	}
	return out
}

// Resolve — найти чат в белом списке по alias, id или username. Иначе — отказ.
func (s *Settings) Resolve(chat string) (ChatRule, error) {
	key := strings.TrimSpace(chat)
	if key == "" {
		return ChatRule{}, &Denied{"Не указан чат."}
	}
	low := strings.ToLower(key)
	for _, a := range s.ChatOrder {
		if strings.ToLower(a) == low {
			return s.Chats[a], nil
		}
	}
	for _, a := range s.ChatOrder {
		rule := s.Chats[a]
		peer := strings.ToLower(rule.PeerString())
		if low == peer || strings.TrimLeft(low, "@") == strings.TrimLeft(peer, "@") {
			return rule, nil
		}
	}
	known := append([]string(nil), s.ChatOrder...)
	sort.Strings(known)
	list := strings.Join(known, ", ")
	if list == "" {
		list = "(список пуст)"
	}
	return ChatRule{}, &Denied{fmt.Sprintf(
		"Чат '%s' не в белом списке, доступ запрещён. Разрешённые алиасы: %s. "+
			"Добавить чат может только человек — правкой config/chats.toml.", chat, list)}
}

// LoadChats читает белый список.
func LoadChats(path string) (map[string]ChatRule, []string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, cfgErr("Нет файла %s. Скопируй config/chats.example.toml в config/chats.toml "+
			"и перечисли разрешённые чаты.", path)
	}
	if err != nil {
		return nil, nil, err
	}
	var raw struct {
		Chat []map[string]any `toml:"chat"`
	}
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return nil, nil, cfgErr("%s: %v", path, err)
	}
	rules := map[string]ChatRule{}
	var order []string
	seen := map[string]bool{}
	for _, entry := range raw.Chat {
		alias := strings.TrimSpace(fmt.Sprint(valueOr(entry, "alias", "")))
		if alias == "" {
			return nil, nil, cfgErr("%s: у одной из записей [[chat]] нет alias", path)
		}
		id, ok := entry["id"]
		if !ok {
			return nil, nil, cfgErr("%s: у чата '%s' нет поля id", path, alias)
		}
		if seen[strings.ToLower(alias)] {
			return nil, nil, cfgErr("%s: alias '%s' повторяется", path, alias)
		}
		seen[strings.ToLower(alias)] = true
		var peer any
		switch v := id.(type) {
		case int64:
			peer = v
		case string:
			peer = v
		default:
			peer = fmt.Sprint(v)
		}
		rules[alias] = ChatRule{
			Alias: alias,
			Peer:  peer,
			Title: fmt.Sprint(valueOr(entry, "title", alias)),
			Read:  truthy(valueOr(entry, "read", true)),
			Send:  truthy(valueOr(entry, "send", false)),
			Auto:  truthy(valueOr(entry, "auto_send", false)),
			Note:  fmt.Sprint(valueOr(entry, "note", "")),
		}
		order = append(order, alias)
	}
	return rules, order, nil
}

func valueOr(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

func truthy(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case int64:
		return b != 0
	case string:
		return b != ""
	}
	return v != nil
}

// botCredentials — токен бота подтверждений. Свой бот не нужен: по умолчанию
// берём тот же, что шлёт уведомления о хуках Claude Code, из его config.env.
func botCredentials() (string, int64) {
	token := strings.TrimSpace(os.Getenv("TG_BOT_TOKEN"))
	chatID := strings.TrimSpace(os.Getenv("TG_APPROVAL_CHAT_ID"))
	if token != "" && chatID != "" {
		id, _ := strconv.ParseInt(chatID, 10, 64)
		return token, id
	}
	donor := os.Getenv("TG_BOT_ENV_FILE")
	if donor == "" {
		home, _ := os.UserHomeDir()
		donor = filepath.Join(home, ".local", "share", "cc-telegram-notify", "config.env")
	}
	if data, err := os.ReadFile(donor); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			key, value, _ := strings.Cut(line, "=")
			key = strings.TrimSpace(key)
			value = strings.Trim(strings.TrimSpace(value), `'"`)
			switch {
			case key == "NOTIFICATIONS_BOT_TOKEN" && token == "":
				token = value
			case key == "NOTIFICATIONS_CHAT_ID" && chatID == "":
				chatID = value
			}
		}
	}
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		id = 0
	}
	return token, id
}

func parseProxy(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "socks5://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, cfgErr("TG_PROXY: ожидается вид socks5://host:port")
	}
	switch u.Scheme {
	case "socks5", "socks4", "http":
	default:
		return nil, cfgErr("TG_PROXY: неизвестная схема '%s'", u.Scheme)
	}
	if u.Hostname() == "" || u.Port() == "" {
		return nil, cfgErr("TG_PROXY: ожидается вид socks5://host:port")
	}
	return u, nil
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// Load читает .env и chats.toml. requireCredentials=false — для doctor.
func Load() (*Settings, error) { return load(true) }

func load(requireCredentials bool) (*Settings, error) {
	applyDotenv(filepath.Join(Root, ".env"))
	apiID := strings.TrimSpace(os.Getenv("TG_API_ID"))
	apiHash := strings.TrimSpace(os.Getenv("TG_API_HASH"))
	if requireCredentials && (apiID == "" || apiHash == "") {
		return nil, cfgErr("Не заданы TG_API_ID / TG_API_HASH. Скопируй .env.example в .env " +
			"и впиши значения с https://my.telegram.org")
	}
	id := 0
	if apiID != "" {
		n, err := strconv.Atoi(apiID)
		if err != nil {
			return nil, cfgErr("TG_API_ID должен быть числом")
		}
		id = n
	}

	session := os.Getenv("TG_SESSION")
	if session == "" {
		session = "data/session.json"
	}
	if !filepath.IsAbs(session) {
		session = filepath.Join(Root, session)
	}
	_ = os.MkdirAll(filepath.Dir(session), 0o700)

	policy := strings.ToLower(strings.TrimSpace(os.Getenv("TG_SEND_POLICY")))
	if policy == "" {
		policy = "bot_approval"
	}
	switch policy {
	case "bot_approval", "human_approval", "agent_confirm", "disabled":
	default:
		return nil, cfgErr("TG_SEND_POLICY: недопустимое значение '%s'", policy)
	}

	token, chatID := botCredentials()
	if policy == "bot_approval" && (token == "" || chatID == 0) {
		return nil, cfgErr("TG_SEND_POLICY=bot_approval, но не найден бот для подтверждений. " +
			"Задай TG_BOT_TOKEN и TG_APPROVAL_CHAT_ID в .env либо путь к чужому " +
			"config.env в TG_BOT_ENV_FILE.")
	}
	proxy, err := parseProxy(os.Getenv("TG_PROXY"))
	if err != nil {
		return nil, err
	}
	chats, order, err := LoadChats(filepath.Join(Root, "config", "chats.toml"))
	if err != nil {
		return nil, err
	}
	return &Settings{
		APIID:              id,
		APIHash:            apiHash,
		Phone:              strings.TrimSpace(os.Getenv("TG_PHONE")),
		SessionPath:        session,
		SendPolicy:         policy,
		DraftTTLMin:        envInt("TG_DRAFT_TTL_MIN", 60),
		Proxy:              proxy,
		MaxLimit:           envInt("TG_MAX_LIMIT", 200),
		BotToken:           token,
		ApprovalChatID:     chatID,
		BotProxy:           strings.TrimSpace(os.Getenv("TG_BOT_PROXY")),
		ApprovalTimeoutSec: envInt("TG_APPROVAL_TIMEOUT_SEC", 600),
		TranscribePerCall:  envInt("TG_TRANSCRIBE_PER_CALL", 10),
		MaxFileMB:          envInt("TG_MAX_FILE_MB", 200),
		MaxDownloadMB:      envInt("TG_MAX_DOWNLOAD_MB", 1024),
		Reactions:          strings.ToLower(strings.TrimSpace(os.Getenv("TG_REACTIONS"))) != "off",
		AutoSendDelaySec:   envInt("TG_AUTO_SEND_DELAY_SEC", 30),
		Chats:              chats,
		ChatOrder:          order,
	}, nil
}

// CLIHint — как владельцу запустить эту же программу из терминала.
func CLIHint() string {
	exe, err := os.Executable()
	if err != nil {
		return "tg"
	}
	if strings.ContainsRune(exe, ' ') {
		return "& \"" + exe + "\""
	}
	return exe
}

// переменные, заданные до запуска процесса: они главнее .env
var startupEnv = func() map[string]bool {
	m := map[string]bool{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		m[k] = true
	}
	return m
}()

// applyDotenv перечитывает .env на каждом вызове: долгоживущие процессы
// (служба, окно управления) подхватывают правки без перезапуска.
func applyDotenv(path string) {
	values, err := godotenv.Read(path)
	if err != nil {
		return
	}
	for k, v := range values {
		if !startupEnv[k] {
			_ = os.Setenv(k, v)
		}
	}
	dotenvMu.Lock()
	defer dotenvMu.Unlock()
	for k := range dotenvKeys {
		if _, still := values[k]; !still && !startupEnv[k] {
			_ = os.Unsetenv(k)
		}
	}
	dotenvKeys = map[string]bool{}
	for k := range values {
		dotenvKeys[k] = true
	}
}

var (
	dotenvMu   sync.Mutex
	dotenvKeys = map[string]bool{}
)

// Build — метка сборки (scripts\build.ps1 подставляет коммит и время).
var Build = "dev"
