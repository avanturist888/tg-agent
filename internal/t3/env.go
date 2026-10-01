package t3

// Окружение T3 Code — один сервер T3 (`t3 serve`): локальный десктоп или
// контейнер со своим сервером. С каждым служба говорит по HTTP API T3:
// заводит треды (thread.create), начинает в них ходы (thread.turn.start) и
// опрашивает снимок треда, пока ход не закончится.
//
// Токен: служба хранит выпущенный ею токен в data/ (0600) и выпускает новый,
// когда старый истекает или сервер ответил 401. Новый токен — это обмен
// одноразового pairing-кода на /oauth/token с правами только на треды;
// код даёт команда (pairing_cmd, например `docker exec … t3 auth pairing
// create --json`) или сам сервер по admin-токену (POST /api/auth/pairing-token).
// Вместо выпуска можно дать готовый токен файлом (token_file).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Ошибки окружения.
var (
	// ThreadGone — треда больше нет: удалён (404) или убран в архив.
	ThreadGone = errors.New("тред удалён или в архиве")
	// Busy — в треде идёт ход: новый не начинаем, пока он не закончится.
	Busy = errors.New("в треде идёт ход")
	// TurnTimeout — ход не закончился за отведённое время.
	TurnTimeout = errors.New("ход не закончился вовремя")
)

const (
	agentInstance   = "claudeAgent" // провайдер Claude Code в T3
	defaultPoll     = 2500 * time.Millisecond
	defaultTurnWait = 15 * time.Minute
)

// Env — сервер T3 Code, с которым говорит служба.
type Env struct {
	Name   string // имя из config/t3.toml; "local" — десктоп этой машины
	Origin string // http://127.0.0.1:3774

	TokenPath  string   // выпущенный службой токен (data/t3-token-<имя>.json)
	TokenFile  string   // готовый токен владельца: {"token": "…"} или строка
	PairingCmd []string // команда, печатающая JSON с полем credential
	AdminToken string   // токен с access:write: код выдаёт сам сервер

	Model       string // модель ходов в тредах шлюза (instanceId claudeAgent)
	Project     string // проект тредов шлюза: workspaceRoot, id или название; "" — первый
	PollEvery   time.Duration
	TurnTimeout time.Duration

	// местный T3: код выдаёт его CLI, старый токен отзывается им же
	pairing func(ctx context.Context) (string, error)
	revoke  func(ctx context.Context, sid string)
}

// ── HTTP ─────────────────────────────────────────────────────────────────

type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string { return fmt.Sprintf("T3 ответил %d: %s", e.code, e.body) }

// StatusCode — HTTP-код ответа T3 (0 — ошибка не от сервера).
func StatusCode(err error) int {
	var st *statusError
	if errors.As(err, &st) {
		return st.code
	}
	return 0
}

// call — запрос к серверу T3. body: url.Values — форма, иначе JSON.
func (e *Env) call(ctx context.Context, method, path, tok string, body, out any) error {
	var rd io.Reader
	ctype := ""
	switch b := body.(type) {
	case nil:
	case url.Values:
		rd, ctype = strings.NewReader(b.Encode()), "application/x-www-form-urlencoded"
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return err
		}
		rd, ctype = bytes.NewReader(raw), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(e.Origin, "/")+path, rd)
	if err != nil {
		return err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		return &statusError{code: resp.StatusCode, body: tail(string(raw), 300)}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// api — запрос с токеном; на 401 токен перевыпускается и запрос повторяется.
func (e *Env) api(ctx context.Context, method, path string, body, out any) error {
	tok, err := e.token(ctx, "")
	if err != nil {
		return err
	}
	err = e.call(ctx, method, path, tok, body, out)
	if StatusCode(err) != http.StatusUnauthorized {
		return err
	}
	// токен отозвали или он истёк — выпускаем новый
	if tok, err = e.token(ctx, tok); err != nil {
		return err
	}
	return e.call(ctx, method, path, tok, body, out)
}

// Descriptor — /.well-known/t3/environment: отвечает ли сервер и тот ли протокол.
func (e *Env) Descriptor(ctx context.Context) (label, version string, err error) {
	var d struct {
		Label    string `json:"label"`
		Version  string `json:"serverVersion"`
		Protocol int    `json:"orchestrationProtocolVersion"`
	}
	if err := e.call(ctx, http.MethodGet, "/.well-known/t3/environment", "", nil, &d); err != nil {
		return "", "", fmt.Errorf("%w: %v", Unavailable, err)
	}
	if d.Protocol != protocolVersion {
		return d.Label, d.Version, fmt.Errorf("%w: протокол T3 %d, а не %d", Unavailable, d.Protocol, protocolVersion)
	}
	return d.Label, d.Version, nil
}

// ── токен ────────────────────────────────────────────────────────────────

type savedToken struct {
	Origin  string    `json:"origin"`
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

var issueMu sync.Map // TokenPath → *sync.Mutex: выпуск токена по очереди

func (e *Env) lockIssue() func() {
	m, _ := issueMu.LoadOrStore(e.TokenPath, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// CanIssue — есть ли чем выпустить новый токен.
func (e *Env) CanIssue() bool {
	return e.pairing != nil || len(e.PairingCmd) > 0 || e.AdminToken != ""
}

// saved — токен, который служба выпустила раньше (может быть от прежнего
// адреса сервера: местный T3 меняет порт).
func (e *Env) saved() savedToken {
	var t savedToken
	if e.TokenPath == "" {
		return t
	}
	if raw, err := os.ReadFile(e.TokenPath); err == nil {
		_ = json.Unmarshal(raw, &t)
	}
	return t
}

// fileToken — токен из token_file: JSON {"token", "origin"} или сама строка.
func (e *Env) fileToken() string {
	if e.TokenFile == "" {
		return ""
	}
	raw, err := os.ReadFile(e.TokenFile)
	if err != nil {
		return ""
	}
	var t struct {
		Origin string `json:"origin"`
		Token  string `json:"token"`
	}
	if json.Unmarshal(raw, &t) == nil {
		if t.Origin != "" && strings.TrimRight(t.Origin, "/") != strings.TrimRight(e.Origin, "/") {
			return "" // токен другого сервера
		}
		return strings.TrimSpace(t.Token)
	}
	tok := strings.TrimSpace(string(raw))
	if strings.ContainsAny(tok, " \n\t{") {
		return ""
	}
	return tok
}

// token — действующий токен. rejected — тот, на который сервер ответил 401:
// его не берём и выпускаем новый.
func (e *Env) token(ctx context.Context, rejected string) (string, error) {
	unlock := e.lockIssue()
	defer unlock()
	old := e.saved()
	usable := old.Token != "" && old.Token != rejected && old.Origin == e.Origin
	if usable && (time.Until(old.Expires) > renewBefore || !e.CanIssue()) {
		return old.Token, nil
	}
	if ft := e.fileToken(); ft != "" && ft != rejected && !usable {
		return ft, nil
	}
	if !e.CanIssue() {
		if rejected != "" {
			return "", fmt.Errorf("T3 %s отклонил токен (401), а выпустить новый нечем: задай pairing_cmd или admin_token_env в config/t3.toml или обнови token_file", e.Name)
		}
		return "", fmt.Errorf("для T3 %s нет токена: задай token_file, pairing_cmd или admin_token_env в config/t3.toml", e.Name)
	}
	code, err := e.pairingCode(ctx)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":        {code},
		"subject_token_type":   {"urn:t3:params:oauth:token-type:environment-bootstrap"},
		"requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"scope":                {tokenScopes},
		"client_label":         {tokenLabel},
	}
	var res struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := e.call(ctx, http.MethodPost, "/oauth/token", "", form, &res); err != nil {
		return "", fmt.Errorf("токен T3 %s: %w", e.Name, err)
	}
	if res.AccessToken == "" {
		return "", fmt.Errorf("токен T3 %s: пустой ответ", e.Name)
	}
	tok := savedToken{Origin: e.Origin, Token: res.AccessToken, Expires: time.Now().Add(time.Duration(res.ExpiresIn) * time.Second)}
	if e.TokenPath != "" {
		raw, _ := json.MarshalIndent(tok, "", "  ")
		if err := writePrivate(e.TokenPath, raw); err != nil {
			return "", err
		}
	}
	if e.revoke != nil {
		if sid := tokenSession(old.Token); sid != "" {
			e.revoke(ctx, sid)
		}
	}
	return tok.Token, nil
}

// pairingCode — одноразовый код (живёт минуты): командой, у сервера по
// admin-токену или у CLI местного T3.
func (e *Env) pairingCode(ctx context.Context) (string, error) {
	switch {
	case e.pairing != nil:
		return e.pairing(ctx)
	case len(e.PairingCmd) > 0:
		return e.pairingFromCmd(ctx)
	case e.AdminToken != "":
		var res struct {
			Credential string `json:"credential"`
		}
		body := map[string]any{"label": tokenLabel, "scopes": strings.Fields(tokenScopes)}
		if err := e.call(ctx, http.MethodPost, "/api/auth/pairing-token", e.AdminToken, body, &res); err != nil {
			return "", fmt.Errorf("код доступа T3 %s по admin-токену: %w", e.Name, err)
		}
		if res.Credential == "" {
			return "", fmt.Errorf("T3 %s не выдал код доступа", e.Name)
		}
		return res.Credential, nil
	}
	return "", fmt.Errorf("для T3 %s не задан способ выпуска токена", e.Name)
}

func (e *Env) pairingFromCmd(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.PairingCmd[0], e.PairingCmd[1:]...)
	hideWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("pairing_cmd T3 %s: %v %s", e.Name, err, tail(stderr.String(), 300))
	}
	code := credential(out)
	if code == "" {
		return "", fmt.Errorf("pairing_cmd T3 %s не напечатала JSON с credential", e.Name)
	}
	return code, nil
}

// credential — поле credential из вывода команды (до JSON может быть мусор).
func credential(out []byte) string {
	if i := bytes.IndexByte(out, '{'); i >= 0 {
		out = out[i:]
	}
	var p struct {
		Credential string `json:"credential"`
	}
	if json.NewDecoder(bytes.NewReader(out)).Decode(&p) != nil {
		return ""
	}
	return p.Credential
}

func writePrivate(path string, raw []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ── треды ────────────────────────────────────────────────────────────────

// Project — проект окружения.
type Project struct {
	ID                    string          `json:"id"`
	Title                 string          `json:"title"`
	WorkspaceRoot         string          `json:"workspaceRoot"`
	DefaultModelSelection json.RawMessage `json:"defaultModelSelection"`
}

// ThreadShell — тред в списке окружения.
type ThreadShell struct {
	ID         string  `json:"id"`
	ProjectID  string  `json:"projectId"`
	Title      string  `json:"title"`
	ArchivedAt *string `json:"archivedAt"`
	LatestTurn *Turn   `json:"latestTurn"`
}

// Shell — проекты и треды окружения (GET /api/orchestration/shell).
type Shell struct {
	Projects []Project     `json:"projects"`
	Threads  []ThreadShell `json:"threads"`
}

// Turn — последний ход треда.
type Turn struct {
	ID                 string  `json:"turnId"`
	State              string  `json:"state"` // running | interrupted | completed | error
	AssistantMessageID *string `json:"assistantMessageId"`
}

// Done — ход закончился (успешно или нет).
func (t *Turn) Done() bool {
	return t != nil && (t.State == "completed" || t.State == "error" || t.State == "interrupted")
}

// Message — сообщение треда.
type Message struct {
	ID        string  `json:"id"`
	Role      string  `json:"role"`
	Text      string  `json:"text"`
	TurnID    *string `json:"turnId"`
	Streaming bool    `json:"streaming"`
}

// Thread — снимок треда (GET /api/orchestration/threads/:id).
type Thread struct {
	ID              string  `json:"id"`
	ProjectID       string  `json:"projectId"`
	Title           string  `json:"title"`
	RuntimeMode     string  `json:"runtimeMode"`
	InteractionMode string  `json:"interactionMode"`
	ArchivedAt      *string `json:"archivedAt"`
	DeletedAt       *string `json:"deletedAt"`
	LatestTurn      *Turn   `json:"latestTurn"`
	Session         *struct {
		Status    string  `json:"status"`
		LastError *string `json:"lastError"`
	} `json:"session"`
	Messages []Message `json:"messages"`
}

// Shell — проекты и треды окружения.
func (e *Env) Shell(ctx context.Context) (*Shell, error) {
	var sh Shell
	if err := e.api(ctx, http.MethodGet, "/api/orchestration/shell", nil, &sh); err != nil {
		return nil, err
	}
	return &sh, nil
}

// Thread — снимок треда с последними ходами. ThreadGone — треда нет.
func (e *Env) Thread(ctx context.Context, id string) (*Thread, error) {
	var snap struct {
		Thread *Thread `json:"thread"`
	}
	// последние два хода: весь тред шлюза за месяцы опрашивать незачем
	err := e.api(ctx, http.MethodGet, "/api/orchestration/threads/"+url.PathEscape(id)+"?turnLimit=2", nil, &snap)
	if StatusCode(err) == http.StatusNotFound {
		return nil, ThreadGone
	}
	if err != nil {
		return nil, err
	}
	if snap.Thread == nil || snap.Thread.DeletedAt != nil {
		return nil, ThreadGone
	}
	return snap.Thread, nil
}

// PickProject — проект, в котором шлюз заводит треды.
func (e *Env) PickProject(sh *Shell) (*Project, error) { return e.project(sh) }

func (e *Env) project(sh *Shell) (*Project, error) {
	if len(sh.Projects) == 0 {
		return nil, fmt.Errorf("в окружении T3 %s нет ни одного проекта (t3 project add …)", e.Name)
	}
	if e.Project == "" {
		return &sh.Projects[0], nil
	}
	for i, p := range sh.Projects {
		if p.ID == e.Project || p.WorkspaceRoot == e.Project || p.Title == e.Project {
			return &sh.Projects[i], nil
		}
	}
	return nil, fmt.Errorf("в окружении T3 %s нет проекта %q", e.Name, e.Project)
}

func (e *Env) modelSelection(p *Project) (any, error) {
	if e.Model != "" {
		return map[string]string{"instanceId": agentInstance, "model": e.Model}, nil
	}
	if len(p.DefaultModelSelection) > 0 && string(p.DefaultModelSelection) != "null" {
		return p.DefaultModelSelection, nil
	}
	return nil, fmt.Errorf("для окружения T3 %s не задана model в config/t3.toml, а у проекта нет модели по умолчанию", e.Name)
}

// CreateThread — новый тред в проекте окружения; режим full-access.
func (e *Env) CreateThread(ctx context.Context, title string) (string, error) {
	sh, err := e.Shell(ctx)
	if err != nil {
		return "", err
	}
	p, err := e.project(sh)
	if err != nil {
		return "", err
	}
	model, err := e.modelSelection(p)
	if err != nil {
		return "", err
	}
	id := randomID()
	cmd := map[string]any{
		"type":            "thread.create",
		"commandId":       randomID(),
		"threadId":        id,
		"projectId":       p.ID,
		"title":           title,
		"modelSelection":  model,
		"runtimeMode":     "full-access",
		"interactionMode": "default",
		"branch":          nil,
		"worktreePath":    nil,
		"createdAt":       isoNow(),
	}
	if err := e.api(ctx, http.MethodPost, "/api/orchestration/dispatch", cmd, nil); err != nil {
		return "", fmt.Errorf("thread.create в T3 %s: %w", e.Name, err)
	}
	return id, nil
}

// StartTurn — начать в треде ход с текстом text. Возвращает id прошлого
// хода: по нему WaitTurn отличит новый ход от старого. Busy — в треде уже
// идёт ход, ThreadGone — треда нет или он в архиве.
func (e *Env) StartTurn(ctx context.Context, thread, text string) (prev string, err error) {
	return e.startTurn(ctx, thread, text, false)
}

func (e *Env) startTurn(ctx context.Context, thread, text string, force bool) (string, error) {
	th, err := e.Thread(ctx, thread)
	if err != nil {
		return "", err
	}
	if th.ArchivedAt != nil {
		return "", ThreadGone
	}
	prev := ""
	if th.LatestTurn != nil {
		prev = th.LatestTurn.ID
		if !force && th.LatestTurn.State == "running" {
			return prev, Busy
		}
	}
	cmd := map[string]any{
		"type":      "thread.turn.start",
		"commandId": randomID(),
		"threadId":  thread,
		"message": map[string]any{
			"messageId":   randomID(),
			"role":        "user",
			"text":        text,
			"attachments": []any{},
		},
		"runtimeMode":     orDefault(th.RuntimeMode, "full-access"),
		"interactionMode": orDefault(th.InteractionMode, "default"),
		"createdAt":       isoNow(),
	}
	if e.Model != "" {
		cmd["modelSelection"] = map[string]string{"instanceId": agentInstance, "model": e.Model}
	}
	if err := e.api(ctx, http.MethodPost, "/api/orchestration/dispatch", cmd, nil); err != nil {
		return prev, err
	}
	return prev, nil
}

// TurnResult — чем закончился ход.
type TurnResult struct {
	TurnID string `json:"turn_id"`
	State  string `json:"state"` // completed | error | interrupted
	Text   string `json:"text"`  // итоговый ответ ассистента
	Error  string `json:"error,omitempty"`
}

// WaitTurn — ждать, пока в треде закончится ход новее prev. Опрашивает
// снимок треда раз в PollEvery, не дольше TurnTimeout.
func (e *Env) WaitTurn(ctx context.Context, thread, prev string) (*TurnResult, error) {
	every, limit := e.PollEvery, e.TurnTimeout
	if every <= 0 {
		every = defaultPoll
	}
	if limit <= 0 {
		limit = defaultTurnWait
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	start := time.Now()
	state := "не начался"
	for {
		th, err := e.Thread(ctx, thread)
		switch {
		case err == nil:
			t := th.LatestTurn
			if t != nil && t.ID != prev {
				state = t.State
				if t.Done() {
					res := &TurnResult{TurnID: t.ID, State: t.State, Text: answer(th, t)}
					if t.State != "completed" && th.Session != nil && th.Session.LastError != nil {
						res.Error = *th.Session.LastError
					}
					return res, nil
				}
			} else if th.Session != nil && th.Session.Status == "error" && time.Since(start) > 15*time.Second {
				// ход так и не начался: провайдер упал на старте
				msg := "сессия провайдера в ошибке"
				if th.Session.LastError != nil {
					msg = *th.Session.LastError
				}
				return &TurnResult{State: "error", Error: msg}, nil
			}
		case errors.Is(err, ThreadGone):
			return nil, err
		case ctx.Err() == nil:
			// сервер моргнул — опрашиваем дальше до таймаута
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("%w (%s): последнее состояние — %s", TurnTimeout, limit, state)
			}
			return nil, ctx.Err()
		case <-time.After(every):
		}
	}
}

// answer — итоговый текст ассистента в ходе t.
func answer(th *Thread, t *Turn) string {
	if t.AssistantMessageID != nil {
		for _, m := range th.Messages {
			if m.ID == *t.AssistantMessageID {
				return m.Text
			}
		}
	}
	for i := len(th.Messages) - 1; i >= 0; i-- {
		m := th.Messages[i]
		if m.Role == "assistant" && m.TurnID != nil && *m.TurnID == t.ID {
			return m.Text
		}
	}
	return ""
}

func isoNow() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
