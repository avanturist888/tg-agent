package t3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeT3 — сервер T3 в памяти: токены, треды, ходы. Ход заканчивается сам
// после finishAfter опросов снимка; до первого опроса нового хода снимок
// ещё lagPolls раз показывает прошлый ход — как настоящий T3.
type fakeT3 struct {
	t           *testing.T
	mu          sync.Mutex
	codes       map[string]bool // действующие pairing-коды
	tokens      map[string]bool // действующие bearer-токены
	admin       string
	issued      int
	exchanges   []map[string]string // формы /oauth/token
	threads     map[string]*fakeThread
	commands    []map[string]any
	finishAfter int    // опросов до конца хода (0 — ход не кончается)
	finalState  string // чем кончается ход
	lagPolls    int
}

type fakeThread struct {
	id, title, runtime string
	archived           bool
	turns              []*fakeTurn
	messages           []map[string]any
}

type fakeTurn struct {
	id, state string
	polls     int
	lag       int
}

func newFakeT3(t *testing.T) (*fakeT3, *httptest.Server) {
	f := &fakeT3{t: t, codes: map[string]bool{}, tokens: map[string]bool{}, threads: map[string]*fakeThread{},
		finishAfter: 2, finalState: "completed"}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeT3) authorized(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && f.tokens[tok]
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeT3) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/.well-known/t3/environment":
		writeJSON(w, 200, map[string]any{"label": "fake", "serverVersion": "0.0.44", "orchestrationProtocolVersion": 1})
	case r.URL.Path == "/oauth/token" && r.Method == http.MethodPost:
		_ = r.ParseForm()
		form := map[string]string{}
		for k := range r.PostForm {
			form[k] = r.PostForm.Get(k)
		}
		f.exchanges = append(f.exchanges, form)
		code := form["subject_token"]
		if !f.codes[code] {
			writeJSON(w, 401, map[string]string{"error": "invalid_credential"})
			return
		}
		delete(f.codes, code) // код одноразовый
		f.issued++
		tok := fmt.Sprintf("tok-%d", f.issued)
		f.tokens[tok] = true
		writeJSON(w, 200, map[string]any{"access_token": tok, "expires_in": 30 * 24 * 3600, "token_type": "Bearer"})
	case r.URL.Path == "/api/auth/pairing-token" && r.Method == http.MethodPost:
		if r.Header.Get("Authorization") != "Bearer "+f.admin || f.admin == "" {
			writeJSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		var body struct {
			Label  string   `json:"label"`
			Scopes []string `json:"scopes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.Join(body.Scopes, " ") != tokenScopes {
			writeJSON(w, 400, map[string]string{"error": "scopes " + strings.Join(body.Scopes, " ")})
			return
		}
		code := fmt.Sprintf("admin-code-%d", len(f.codes)+f.issued)
		f.codes[code] = true
		writeJSON(w, 200, map[string]any{"id": "p1", "credential": code, "label": body.Label})
	case !f.authorized(r):
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
	case r.URL.Path == "/api/orchestration/shell":
		var threads []map[string]any
		for _, th := range f.threads {
			threads = append(threads, map[string]any{"id": th.id, "projectId": "p-main", "title": th.title})
		}
		writeJSON(w, 200, map[string]any{
			"projects": []map[string]any{
				{"id": "p-other", "title": "other", "workspaceRoot": "/other", "defaultModelSelection": nil},
				{"id": "p-main", "title": "workspace", "workspaceRoot": "/workspace", "defaultModelSelection": nil},
			},
			"threads": threads,
		})
	case strings.HasPrefix(r.URL.Path, "/api/orchestration/threads/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/orchestration/threads/")
		th := f.threads[id]
		if th == nil {
			writeJSON(w, 404, map[string]string{"error": "thread_not_found"})
			return
		}
		writeJSON(w, 200, map[string]any{"snapshotSequence": 1, "thread": f.snapshot(th)})
	case r.URL.Path == "/api/orchestration/dispatch" && r.Method == http.MethodPost:
		var cmd map[string]any
		_ = json.NewDecoder(r.Body).Decode(&cmd)
		f.commands = append(f.commands, cmd)
		switch cmd["type"] {
		case "thread.create":
			id := cmd["threadId"].(string)
			f.threads[id] = &fakeThread{id: id, title: cmd["title"].(string), runtime: cmd["runtimeMode"].(string)}
		case "thread.turn.start":
			th := f.threads[cmd["threadId"].(string)]
			if th == nil {
				writeJSON(w, 500, map[string]string{"error": "Thread does not exist"})
				return
			}
			msg := cmd["message"].(map[string]any)
			th.messages = append(th.messages, map[string]any{"id": msg["messageId"], "role": "user", "text": msg["text"], "turnId": nil, "streaming": false})
			th.turns = append(th.turns, &fakeTurn{id: randomID(), state: "running", lag: f.lagPolls})
		}
		writeJSON(w, 200, map[string]any{"sequence": len(f.commands)})
	default:
		writeJSON(w, 404, map[string]string{"error": "not_found"})
	}
}

// snapshot — снимок треда; каждый опрос двигает идущий ход.
func (f *fakeT3) snapshot(th *fakeThread) map[string]any {
	out := map[string]any{"id": th.id, "projectId": "p-main", "title": th.title, "runtimeMode": th.runtime,
		"interactionMode": "default", "latestTurn": nil, "session": map[string]any{"status": "ready", "lastError": nil}}
	if th.archived {
		out["archivedAt"] = "2026-10-01T00:00:00.000Z"
	}
	turns := th.turns
	if n := len(turns); n > 0 && turns[n-1].lag > 0 {
		turns[n-1].lag--
		turns = turns[:n-1] // новый ход ещё не виден
	}
	if n := len(turns); n > 0 {
		cur := turns[n-1]
		if cur.state == "running" {
			cur.polls++
			if f.finishAfter > 0 && cur.polls >= f.finishAfter {
				cur.state = f.finalState
				aid := "assistant:" + cur.id
				th.messages = append(th.messages, map[string]any{"id": aid, "role": "assistant", "text": "готов", "turnId": cur.id, "streaming": false})
				if cur.state == "error" {
					out["session"] = map[string]any{"status": "error", "lastError": "провайдер упал"}
				}
			}
		}
		lt := map[string]any{"turnId": cur.id, "state": cur.state, "assistantMessageId": nil}
		if cur.state != "running" {
			lt["assistantMessageId"] = "assistant:" + cur.id
		}
		out["latestTurn"] = lt
	}
	out["messages"] = th.messages
	return out
}

func (f *fakeT3) newCode() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	code := fmt.Sprintf("code-%d", len(f.codes)+f.issued+100)
	f.codes[code] = true
	return code
}

func (f *fakeT3) revokeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.tokens)
}

func (f *fakeT3) lastCommand(kind string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.commands) - 1; i >= 0; i-- {
		if f.commands[i]["type"] == kind {
			return f.commands[i]
		}
	}
	return nil
}

// testEnv — окружение на поддельном сервере; код выдаёт функция (как pairing_cmd).
func testEnv(t *testing.T, f *fakeT3, srv *httptest.Server) *Env {
	return &Env{Name: "fake", Origin: srv.URL, TokenPath: filepath.Join(t.TempDir(), "t3-token-fake.json"),
		Model: "claude-sonnet-5-5", Project: "/workspace", PollEvery: 10 * time.Millisecond, TurnTimeout: 5 * time.Second,
		pairing: func(context.Context) (string, error) { return f.newCode(), nil }}
}

// ── pairing_cmd: сам тестовый бинарник печатает код ─────────────────────

func TestMain(m *testing.M) {
	if code := os.Getenv("T3_TEST_PAIRING"); code != "" {
		fmt.Println("Pairing created")
		fmt.Printf(`{"id":"p1","credential":%q,"label":"tg-agent"}`+"\n", code)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestPairingCmdExchange(t *testing.T) {
	f, srv := newFakeT3(t)
	f.codes["cmd-code"] = true
	t.Setenv("T3_TEST_PAIRING", "cmd-code")
	e := testEnv(t, f, srv)
	e.pairing = nil
	exe, _ := os.Executable()
	e.PairingCmd = []string{exe}
	sh, err := e.Shell(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sh.Projects) != 2 {
		t.Fatalf("проекты: %+v", sh.Projects)
	}
	if len(f.exchanges) != 1 {
		t.Fatalf("обменов кода: %d", len(f.exchanges))
	}
	form := f.exchanges[0]
	if form["scope"] != tokenScopes || form["client_label"] != tokenLabel ||
		form["grant_type"] != "urn:ietf:params:oauth:grant-type:token-exchange" || form["subject_token"] != "cmd-code" {
		t.Fatalf("форма обмена: %v", form)
	}
	raw, err := os.ReadFile(e.TokenPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved savedToken
	_ = json.Unmarshal(raw, &saved)
	if saved.Token != "tok-1" || saved.Origin != srv.URL || time.Until(saved.Expires) < 29*24*time.Hour {
		t.Fatalf("сохранённый токен: %+v", saved)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(e.TokenPath); st.Mode().Perm() != 0o600 {
			t.Fatalf("права файла токена: %v", st.Mode())
		}
	}
	// второй вызов — тот же токен, без нового кода
	if _, err := e.Shell(context.Background()); err != nil || f.issued != 1 {
		t.Fatalf("повторный вызов: err=%v, выпущено %d", err, f.issued)
	}
}

func TestAdminPairing(t *testing.T) {
	f, srv := newFakeT3(t)
	f.admin = "admin-secret"
	e := testEnv(t, f, srv)
	e.pairing, e.AdminToken = nil, "admin-secret"
	if _, err := e.Shell(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.issued != 1 || !strings.HasPrefix(f.exchanges[0]["subject_token"], "admin-code-") {
		t.Fatalf("код по admin-токену: %v", f.exchanges)
	}
	// чужой admin-токен — понятная ошибка, а не паника
	e2 := testEnv(t, f, srv)
	e2.pairing, e2.AdminToken = nil, "wrong"
	if _, err := e2.Shell(context.Background()); err == nil || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("неверный admin-токен: %v", err)
	}
}

func TestTokenFile(t *testing.T) {
	f, srv := newFakeT3(t)
	f.tokens["owner-token"] = true
	e := testEnv(t, f, srv)
	e.pairing = nil
	e.TokenFile = filepath.Join(t.TempDir(), "tok.json")
	os.WriteFile(e.TokenFile, []byte(fmt.Sprintf(`{"origin":%q,"token":"owner-token"}`, srv.URL)), 0o600)
	if _, err := e.Shell(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.issued != 0 {
		t.Fatal("с token_file новый токен выпускать не нужно")
	}
	// токен отозвали, выпустить нечем — ошибка подсказывает, что делать
	f.revokeAll()
	_, err := e.Shell(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pairing_cmd") {
		t.Fatalf("401 без способа выпуска: %v", err)
	}
	// строка без JSON тоже годится
	os.WriteFile(e.TokenFile, []byte("plain-token\n"), 0o600)
	f.tokens["plain-token"] = true
	if _, err := e.Shell(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReissueOn401(t *testing.T) {
	f, srv := newFakeT3(t)
	e := testEnv(t, f, srv)
	ctx := context.Background()
	if _, err := e.Shell(ctx); err != nil {
		t.Fatal(err)
	}
	f.revokeAll() // владелец отозвал сессию tg-agent в T3
	id, err := e.CreateThread(ctx, "TG: Избранное")
	if err != nil {
		t.Fatalf("после 401 ждали перевыпуск токена: %v", err)
	}
	if f.issued != 2 || id == "" {
		t.Fatalf("выпущено токенов %d, тред %q", f.issued, id)
	}
	raw, _ := os.ReadFile(e.TokenPath)
	if !strings.Contains(string(raw), "tok-2") {
		t.Fatal("новый токен не сохранён")
	}
}

func TestCreateThreadTurnAndWait(t *testing.T) {
	f, srv := newFakeT3(t)
	f.lagPolls = 2 // T3 сначала показывает прошлый ход
	e := testEnv(t, f, srv)
	ctx := context.Background()
	id, err := e.CreateThread(ctx, "TG: Избранное")
	if err != nil {
		t.Fatal(err)
	}
	cmd := f.lastCommand("thread.create")
	ms, _ := cmd["modelSelection"].(map[string]any)
	if cmd["projectId"] != "p-main" || cmd["runtimeMode"] != "full-access" || cmd["title"] != "TG: Избранное" ||
		ms["instanceId"] != "claudeAgent" || ms["model"] != "claude-sonnet-5-5" || cmd["branch"] != nil {
		t.Fatalf("thread.create: %v", cmd)
	}

	// первый ход
	prev, err := e.StartTurn(ctx, id, "Ответь одним словом: готов")
	if err != nil || prev != "" {
		t.Fatalf("первый ход: prev=%q err=%v", prev, err)
	}
	res, err := e.WaitTurn(ctx, id, prev)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "completed" || res.Text != "готов" || res.TurnID == "" {
		t.Fatalf("итог хода: %+v", res)
	}
	turn := f.lastCommand("thread.turn.start")
	if turn["runtimeMode"] != "full-access" || turn["message"].(map[string]any)["text"] != "Ответь одним словом: готов" {
		t.Fatalf("thread.turn.start: %v", turn)
	}

	// второй ход: прошлый (завершённый) не должен сойти за новый
	prev2, err := e.StartTurn(ctx, id, "ещё раз")
	if err != nil || prev2 != res.TurnID {
		t.Fatalf("второй ход: prev=%q err=%v", prev2, err)
	}
	res2, err := e.WaitTurn(ctx, id, prev2)
	if err != nil || res2.TurnID == res.TurnID || res2.State != "completed" {
		t.Fatalf("второй ход: %+v %v", res2, err)
	}
}

func TestStartTurnBusyAndGone(t *testing.T) {
	f, srv := newFakeT3(t)
	f.finishAfter = 0 // ход не кончается
	e := testEnv(t, f, srv)
	ctx := context.Background()
	id, err := e.CreateThread(ctx, "TG: x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.StartTurn(ctx, id, "раз"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.StartTurn(ctx, id, "два"); !errors.Is(err, Busy) {
		t.Fatalf("ход идёт: ждали Busy, получили %v", err)
	}
	// ожидание упирается в таймаут
	e.TurnTimeout = 100 * time.Millisecond
	if _, err := e.WaitTurn(ctx, id, ""); !errors.Is(err, TurnTimeout) {
		t.Fatalf("бесконечный ход: ждали TurnTimeout, получили %v", err)
	}

	// удалённый тред — 404
	if _, err := e.Thread(ctx, "нет-такого"); !errors.Is(err, ThreadGone) {
		t.Fatalf("404: ждали ThreadGone, получили %v", err)
	}
	if _, err := e.StartTurn(ctx, "нет-такого", "x"); !errors.Is(err, ThreadGone) {
		t.Fatalf("ход в удалённом треде: %v", err)
	}
	// тред в архиве — тоже нет
	f.mu.Lock()
	f.threads[id].archived = true
	f.mu.Unlock()
	if _, err := e.StartTurn(ctx, id, "x"); !errors.Is(err, ThreadGone) {
		t.Fatalf("архив: ждали ThreadGone, получили %v", err)
	}
}

func TestWaitTurnError(t *testing.T) {
	f, srv := newFakeT3(t)
	f.finalState = "error"
	e := testEnv(t, f, srv)
	ctx := context.Background()
	id, _ := e.CreateThread(ctx, "TG: x")
	prev, err := e.StartTurn(ctx, id, "x")
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.WaitTurn(ctx, id, prev)
	if err != nil || res.State != "error" || res.Error != "провайдер упал" {
		t.Fatalf("ход с ошибкой: %+v %v", res, err)
	}
}

func TestProjectAndModel(t *testing.T) {
	f, srv := newFakeT3(t)
	e := testEnv(t, f, srv)
	e.Project = "/nope"
	if _, err := e.CreateThread(context.Background(), "TG: x"); err == nil || !strings.Contains(err.Error(), "/nope") {
		t.Fatalf("нет проекта: %v", err)
	}
	e.Project, e.Model = "", ""
	if _, err := e.CreateThread(context.Background(), "TG: x"); err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("нет модели: %v", err)
	}
}

func TestCredentialParsing(t *testing.T) {
	if got := credential([]byte("Pairing created\n{\"credential\":\"abc\"}\n")); got != "abc" {
		t.Fatalf("credential: %q", got)
	}
	if credential([]byte("мусор")) != "" {
		t.Fatal("из мусора кода нет")
	}
}
