// Package gui — окно управления: белый список чатов, черновики, настройки,
// состояние и вход в Telegram.
//
// Устроено как локальный HTTP-сервер на 127.0.0.1 со случайным портом и
// одноразовым токеном; окно WebView2 показывает его страницу. Токен и
// проверка Host не дают чужим страницам в браузере дёргать API.
package gui

import (
	"bufio"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"tgagent/internal/audit"
	"tgagent/internal/chatsfile"
	"tgagent/internal/config"
	"tgagent/internal/core"
	"tgagent/internal/envfile"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/service"
	"tgagent/internal/tgc"
)

//go:embed static
var static embed.FS

// Server — HTTP-часть окна.
type Server struct {
	token string
	addr  string
	login *loginFlow
}

func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Start поднимает сервер и возвращает адрес страницы.
func Start(ctx context.Context) (*Server, string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	srv := &Server{token: newToken(), addr: ln.Addr().String()}
	mux := http.NewServeMux()
	sub, _ := fs.Sub(static, "static")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	srv.routes(mux)
	hs := &http.Server{Handler: srv.guard(mux), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	go func() {
		<-ctx.Done()
		_ = hs.Close()
	}()
	return srv, fmt.Sprintf("http://%s/#%s", srv.addr, srv.token), nil
}

// guard — только наш Host и, для API, только с токеном.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != s.addr {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Header.Get("X-Token") != s.token {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	raw, err := omap.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	raw, _ := omap.Marshal(omap.New().Set("error", err.Error()))
	_, _ = w.Write(raw)
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/check", s.check)
	mux.HandleFunc("GET /api/chats", s.chats)
	mux.HandleFunc("POST /api/chats", s.addChat)
	mux.HandleFunc("PUT /api/chats/{alias}", s.updateChat)
	mux.HandleFunc("DELETE /api/chats/{alias}", s.removeChat)
	mux.HandleFunc("GET /api/dialogs", s.dialogs)
	mux.HandleFunc("GET /api/drafts", s.drafts)
	mux.HandleFunc("POST /api/drafts/{id}/approve", s.approve)
	mux.HandleFunc("POST /api/drafts/{id}/reject", s.reject)
	mux.HandleFunc("PUT /api/drafts/{id}/text", s.editText)
	mux.HandleFunc("GET /api/settings", s.settings)
	mux.HandleFunc("PUT /api/settings", s.saveSettings)
	mux.HandleFunc("GET /api/audit", s.audit)
	mux.HandleFunc("POST /api/login/start", s.loginStart)
	mux.HandleFunc("GET /api/login/state", s.loginState)
	mux.HandleFunc("POST /api/login/answer", s.loginAnswer)
}

// ── состояние ────────────────────────────────────────────────────────────

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	st, err := config.Load()
	if err != nil {
		writeJSON(w, omap.New().Set("config_error", err.Error()).Set("root", config.Root))
		return
	}
	_, sessErr := os.Stat(st.SessionPath)
	var status service.Status
	up := service.Do(r.Context(), "status", nil, &status) == nil
	out := omap.New().
		Set("root", config.Root).
		Set("exe", config.CLIHint()).
		Set("send_policy", st.SendPolicy).
		Set("session_file", sessErr == nil).
		Set("service", up).
		Set("listener", up && status.Buttons)
	if up {
		out.Set("service_build", status.Build).Set("service_started", status.Started).
			Set("service_connected", status.Connected).Set("service_authorized", status.Authorized)
	}
	writeJSON(w, out.
		Set("bot_ready", st.BotReady()).
		Set("auto_send_delay", st.AutoSendDelaySec).
		Set("chats", len(st.Chats)))
}

// check — живая проверка: вход в Telegram, бот, сдвиг часов.
func (s *Server) check(w http.ResponseWriter, r *http.Request) {
	st, err := config.Load()
	if err != nil {
		writeErr(w, err)
		return
	}
	_ = st
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	var chk service.Check
	if err := service.Do(ctx, "check", nil, &chk); err != nil {
		writeErr(w, err)
		return
	}
	out := omap.New().Set("session", chk.Session).Set("clock_offset_sec", chk.ClockOffset)
	if chk.User != nil {
		out.Set("user", omap.New().Set("id", chk.User.ID).
			Set("name", strings.TrimSpace(chk.User.FirstName+" "+chk.User.LastName)).Set("username", chk.User.Username))
	}
	if chk.SessionError != "" {
		out.Set("session_error", chk.SessionError)
	}
	if chk.Bot != "" {
		out.Set("bot", chk.Bot)
	}
	if chk.BotError != "" {
		out.Set("bot_error", chk.BotError)
	}
	writeJSON(w, out)
}

// ── чаты ─────────────────────────────────────────────────────────────────

func (s *Server) chats(w http.ResponseWriter, r *http.Request) {
	st, err := config.Load()
	if err != nil {
		writeErr(w, err)
		return
	}
	var rows []*omap.Map
	for _, rule := range st.Rules() {
		row := rule.AsMap().Set("id", rule.PeerString())
		if rule.Read {
			row.Set("last_id", core.LastID(core.FeedPath(st, rule.Alias)))
		}
		rows = append(rows, row)
	}
	if rows == nil {
		rows = []*omap.Map{}
	}
	writeJSON(w, rows)
}

type chatIn struct {
	Alias string  `json:"alias"`
	ID    string  `json:"id"`
	Title *string `json:"title"`
	Read  bool    `json:"read"`
	Send  bool    `json:"send"`
	Auto  bool    `json:"auto"`
	Note  *string `json:"note"`
}

func validAlias(a string) bool {
	if a == "" || len(a) > 64 {
		return false
	}
	for _, ch := range a {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.') {
			return false
		}
	}
	return true
}

func (s *Server) addChat(w http.ResponseWriter, r *http.Request) {
	var in chatIn
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if !validAlias(in.Alias) {
		writeErr(w, errors.New("алиас — латиница, цифры, «-», «_», «.»; до 64 символов"))
		return
	}
	var peer any = strings.TrimSpace(in.ID)
	if n, err := strconv.ParseInt(in.ID, 10, 64); err == nil {
		peer = n
	}
	c := chatsfile.Chat{Alias: in.Alias, Peer: peer, Read: in.Read, Send: in.Send, Auto: in.Auto && in.Send}
	if in.Title != nil {
		c.Title = strings.ReplaceAll(*in.Title, `"`, "'")
	}
	if in.Note != nil {
		c.Note = *in.Note
	}
	if err := chatsfile.Add(c); err != nil {
		writeErr(w, err)
		return
	}
	logGUI("gui_chat_add", "alias", c.Alias, "read", c.Read, "send", c.Send, "auto_send", c.Auto)
	writeJSON(w, omap.New().Set("ok", true))
}

func (s *Server) updateChat(w http.ResponseWriter, r *http.Request) {
	var in chatIn
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	alias := r.PathValue("alias")
	if err := chatsfile.Update(alias, in.Read, in.Send, in.Auto && in.Send, in.Title, in.Note); err != nil {
		writeErr(w, err)
		return
	}
	logGUI("gui_chat_update", "alias", alias, "read", in.Read, "send", in.Send, "auto_send", in.Auto && in.Send)
	writeJSON(w, omap.New().Set("ok", true))
}

func (s *Server) removeChat(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	if err := chatsfile.Remove(alias); err != nil {
		writeErr(w, err)
		return
	}
	logGUI("gui_chat_remove", "alias", alias)
	writeJSON(w, omap.New().Set("ok", true))
}

func logGUI(action string, kv ...any) {
	if st, err := config.Load(); err == nil {
		_ = audit.Log(st.AuditPath(), action, kv...)
	}
}

func (s *Server) dialogs(w http.ResponseWriter, r *http.Request) {
	st, err := config.Load()
	if err != nil {
		writeErr(w, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 300
	}
	needle := strings.ToLower(r.URL.Query().Get("filter"))
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	var rows []tgc.DialogRow
	if err := service.Do(ctx, "dialogs", service.LimitArgs{Limit: limit}, &rows); err != nil {
		writeErr(w, err)
		return
	}
	known := map[string]string{}
	for _, rule := range st.Rules() {
		known[rule.PeerString()] = rule.Alias
	}
	out := []*omap.Map{}
	for _, d := range rows {
		if needle != "" && !strings.Contains(strings.ToLower(d.Title), needle) &&
			!strings.Contains(strings.ToLower(d.Username), needle) {
			continue
		}
		id := strconv.FormatInt(d.ID, 10)
		out = append(out, omap.New().Set("id", id).Set("title", d.Title).Set("username", d.Username).
			Set("kind", d.Kind).Set("unread", d.Unread).Set("archived", d.Archived).Set("alias", known[id]))
	}
	writeJSON(w, out)
}

// ── черновики ────────────────────────────────────────────────────────────

func (s *Server) drafts(w http.ResponseWriter, r *http.Request) {
	st, err := config.Load()
	if err != nil {
		writeErr(w, err)
		return
	}
	drafts, err := outbox.New(st.OutboxPath()).List("")
	if err != nil {
		writeErr(w, err)
		return
	}
	out := []*omap.Map{}
	for i := len(drafts) - 1; i >= 0; i-- { // свежие сверху
		d := drafts[i]
		files := []string{}
		for _, f := range d.Files {
			files = append(files, f.Name)
		}
		row := omap.New().Set("id", d.ID).Set("chat", d.Chat).Set("status", d.Status).Set("text", d.Text).
			Set("note", d.Note).Set("fmt", d.Fmt).Set("created_at", d.CreatedAt).Set("expires_at", d.ExpiresAt).
			Set("send_at", d.SendAt).Set("sent_at", d.SentAt).Set("message_id", d.MessageID).
			Set("send_error", d.SendError).Set("approved_by", d.ApprovedBy()).Set("files", files)
		out = append(out, row)
	}
	writeJSON(w, out)
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var out struct {
		Summary string `json:"summary"`
	}
	if err := service.Do(ctx, "approve_send", service.DraftArgs{ID: r.PathValue("id"), By: "gui"}, &out); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, omap.New().Set("summary", out.Summary))
}

func (s *Server) reject(w http.ResponseWriter, r *http.Request) {
	st, err := config.Load()
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := core.CancelDraft(ctx, st, r.PathValue("id"), "gui")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, out)
}

func (s *Server) editText(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	st, err := config.Load()
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := core.EditDraft(ctx, st, r.PathValue("id"), in.Text, "gui"); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, omap.New().Set("ok", true))
}

// ── настройки ────────────────────────────────────────────────────────────

type setting struct {
	Key     string
	Label   string
	Kind    string // int, choice, bool
	Default string
	Choices []string
	Help    string
}

var editable = []setting{
	{"TG_SEND_POLICY", "Режим отправки", "choice", "bot_approval", []string{"bot_approval", "human_approval", "agent_confirm", "disabled"},
		"bot_approval — кнопка в боте; human_approval — команда tg approve; agent_confirm — согласие в диалоге; disabled — только чтение"},
	{"TG_AUTO_SEND_DELAY_SEC", "Окно отмены автоотправки, с", "int", "30", nil, "Сколько секунд у агента есть, чтобы отозвать сообщение в чате с автоотправкой"},
	{"TG_DRAFT_TTL_MIN", "Жизнь черновика, мин", "int", "60", nil, "Неподтверждённый черновик протухает через это время"},
	{"TG_APPROVAL_TIMEOUT_SEC", "Ожидание кнопки агентом, с", "int", "600", nil, "Потолок tg_wait_approval — агент не висит дольше"},
	{"TG_REACTIONS", "Реакции агентов", "choice", "direct", []string{"direct", "off"}, "direct — ставят сразу (только в чатах с отправкой), off — запрещены"},
	{"TG_MAX_LIMIT", "Сообщений за запрос", "int", "200", nil, "Потолок tg_read_chat"},
	{"TG_TRANSCRIBE_PER_CALL", "Расшифровок за чтение", "int", "10", nil, "Сколько голосовых расшифровывать за один tg_read_chat"},
	{"TG_MAX_FILE_MB", "Файл на отправку, МБ", "int", "200", nil, "Предел файла, который агент может отправить"},
	{"TG_MAX_DOWNLOAD_MB", "Файл на скачивание, МБ", "int", "1024", nil, "Предел файла, который агент может скачать"},
}

func envPath() string { return filepath.Join(config.Root, ".env") }

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	values := envfile.Read(envPath())
	out := []*omap.Map{}
	for _, e := range editable {
		v, ok := values[e.Key]
		if !ok {
			v = e.Default
		}
		out = append(out, omap.New().Set("key", e.Key).Set("label", e.Label).Set("kind", e.Kind).
			Set("value", v).Set("default", e.Default).Set("choices", e.Choices).Set("help", e.Help))
	}
	writeJSON(w, out)
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	for key, value := range in {
		var spec *setting
		for i := range editable {
			if editable[i].Key == key {
				spec = &editable[i]
			}
		}
		if spec == nil {
			writeErr(w, fmt.Errorf("настройку %s из окна менять нельзя", key))
			return
		}
		value = strings.TrimSpace(value)
		switch spec.Kind {
		case "int":
			if n, err := strconv.Atoi(value); err != nil || n < 0 {
				writeErr(w, fmt.Errorf("%s: нужно целое неотрицательное число", spec.Label))
				return
			}
		case "choice":
			ok := false
			for _, c := range spec.Choices {
				ok = ok || c == value
			}
			if !ok {
				writeErr(w, fmt.Errorf("%s: недопустимое значение", spec.Label))
				return
			}
		}
		if err := envfile.Set(envPath(), key, value); err != nil {
			writeErr(w, err)
			return
		}
		logGUI("gui_setting", "key", key, "value", value)
	}
	if _, err := config.Load(); err != nil {
		writeErr(w, fmt.Errorf("сохранено, но конфигурация не читается: %w", err))
		return
	}
	writeJSON(w, omap.New().Set("ok", true))
}

// ── журнал ───────────────────────────────────────────────────────────────

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	st, err := config.Load()
	if err != nil {
		writeErr(w, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 2000 {
		limit = 300
	}
	f, err := os.Open(st.AuditPath())
	if err != nil {
		writeJSON(w, []any{})
		return
	}
	defer f.Close()
	info, _ := f.Stat()
	if info.Size() > 2*1024*1024 {
		_, _ = f.Seek(info.Size()-2*1024*1024, 0)
	}
	var lines []json.RawMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if json.Valid(line) {
			lines = append(lines, append(json.RawMessage(nil), line...))
		}
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	writeJSON(w, lines)
}

// ── вход в Telegram ──────────────────────────────────────────────────────

// loginFlow — вход по шагам: окно спрашивает код и пароль по мере надобности.
type loginFlow struct {
	mu      sync.Mutex
	stage   string // phone_sent → need_code → need_password → checking → done / error
	err     string
	user    string
	answers chan string
	service bool // вход идёт через службу (она держит сессию)
}

// step — применить шаг входа от службы.
func (l *loginFlow) step(st tgc.LoginStep, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		l.stage, l.err = "error", err.Error()
		return
	}
	l.stage, l.user = st.Stage, st.User
}

func (l *loginFlow) set(stage string) {
	l.mu.Lock()
	l.stage = stage
	l.mu.Unlock()
}

func (l *loginFlow) wait(ctx context.Context, stage string) (string, error) {
	l.set(stage)
	select {
	case v := <-l.answers:
		return v, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(10 * time.Minute):
		return "", errors.New("не дождался ответа")
	}
}

type guiPrompter struct {
	l     *loginFlow
	phone string
}

func (p guiPrompter) Phone(context.Context) (string, error) { return p.phone, nil }
func (p guiPrompter) Code(ctx context.Context) (string, error) {
	return p.l.wait(ctx, "need_code")
}
func (p guiPrompter) Password(ctx context.Context) (string, error) {
	return p.l.wait(ctx, "need_password")
}

func (s *Server) loginStart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Phone string `json:"phone"`
	}
	_ = decode(r, &in)
	st, err := config.Load()
	if err != nil {
		writeErr(w, err)
		return
	}
	phone := strings.TrimSpace(in.Phone)
	if phone == "" {
		phone = st.Phone
	}
	if phone == "" {
		writeErr(w, errors.New("нужен номер телефона"))
		return
	}
	if s.login != nil {
		s.login.mu.Lock()
		busy := s.login.stage != "done" && s.login.stage != "error"
		s.login.mu.Unlock()
		if busy {
			writeErr(w, errors.New("вход уже идёт"))
			return
		}
	}
	l := &loginFlow{stage: "phone_sent", answers: make(chan string)}
	s.login = l
	if service.Up(r.Context()) {
		l.service = true
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			var step tgc.LoginStep
			err := service.Do(ctx, "login_begin", service.ValueArgs{Value: phone}, &step)
			l.step(step, err)
		}()
		writeJSON(w, omap.New().Set("ok", true))
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		me, err := tgc.Login(ctx, st, guiPrompter{l: l, phone: phone})
		l.mu.Lock()
		defer l.mu.Unlock()
		if err != nil {
			l.stage, l.err = "error", err.Error()
			return
		}
		l.stage = "done"
		l.user = strings.TrimSpace(me.FirstName + " " + me.LastName)
		if me.Username != "" {
			l.user += " @" + me.Username
		}
		_ = audit.Log(st.AuditPath(), "login", "user_id", me.ID, "username", me.Username, "via", "gui")
	}()
	writeJSON(w, omap.New().Set("ok", true))
}

func (s *Server) loginState(w http.ResponseWriter, r *http.Request) {
	if s.login == nil {
		writeJSON(w, omap.New().Set("stage", "idle"))
		return
	}
	s.login.mu.Lock()
	defer s.login.mu.Unlock()
	writeJSON(w, omap.New().Set("stage", s.login.stage).Set("error", s.login.err).Set("user", s.login.user))
}

func (s *Server) loginAnswer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Value string `json:"value"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if s.login == nil {
		writeErr(w, errors.New("вход не начат"))
		return
	}
	if l := s.login; l.service {
		l.mu.Lock()
		l.stage = "checking"
		l.mu.Unlock()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			var step tgc.LoginStep
			err := service.Do(ctx, "login_answer", service.ValueArgs{Value: strings.TrimSpace(in.Value)}, &step)
			l.step(step, err)
		}()
		writeJSON(w, omap.New().Set("ok", true))
		return
	}
	select {
	case s.login.answers <- strings.TrimSpace(in.Value):
		writeJSON(w, omap.New().Set("ok", true))
	case <-time.After(5 * time.Second):
		writeErr(w, errors.New("сейчас ответ не ждут"))
	}
}
