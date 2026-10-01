// Package t3test — сервер T3 Code в памяти для тестов: токены, проекты,
// треды и ходы, которые заканчиваются сами через несколько опросов.
package t3test

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Scopes — права, которые шлюз просит у T3.
const Scopes = "orchestration:read orchestration:operate"

// Fake — поддельный сервер T3. Ход заканчивается после FinishAfter опросов
// снимка треда (0 — не кончается); до этого LagPolls опросов снимок ещё
// показывает прошлый ход — как настоящий T3 сразу после thread.turn.start.
type Fake struct {
	URL string

	mu          sync.Mutex
	codes       map[string]bool
	tokens      map[string]bool
	admin       string
	issued      int
	exchanges   []map[string]string
	threads     map[string]*thread
	order       []string
	commands    []map[string]any
	FinishAfter int
	FinalState  string
	LagPolls    int
	Answer      string
}

type thread struct {
	id, title, runtime string
	archived           bool
	turns              []*turn
	messages           []map[string]any
}

type turn struct {
	id, state string
	polls     int
	lag       int
}

// New — сервер на httptest; закрывается вместе с тестом.
func New(t testing.TB) *Fake {
	f := &Fake{codes: map[string]bool{}, tokens: map[string]bool{}, threads: map[string]*thread{},
		FinishAfter: 2, FinalState: "completed", Answer: "готов"}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

func id() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Fake) authorized(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && f.tokens[tok]
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
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
		if f.admin == "" || r.Header.Get("Authorization") != "Bearer "+f.admin {
			writeJSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		var body struct {
			Label  string   `json:"label"`
			Scopes []string `json:"scopes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.Join(body.Scopes, " ") != Scopes {
			writeJSON(w, 400, map[string]string{"error": "scopes " + strings.Join(body.Scopes, " ")})
			return
		}
		code := "admin-code-" + id()
		f.codes[code] = true
		writeJSON(w, 200, map[string]any{"id": "p1", "credential": code, "label": body.Label})
	case !f.authorized(r):
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
	case r.URL.Path == "/api/orchestration/shell":
		threads := []map[string]any{}
		for _, tid := range f.order {
			if th := f.threads[tid]; th != nil {
				threads = append(threads, map[string]any{"id": th.id, "projectId": "p-main", "title": th.title})
			}
		}
		writeJSON(w, 200, map[string]any{
			"projects": []map[string]any{
				{"id": "p-other", "title": "other", "workspaceRoot": "/other", "defaultModelSelection": nil},
				{"id": "p-main", "title": "workspace", "workspaceRoot": "/workspace", "defaultModelSelection": nil},
			},
			"threads": threads,
		})
	case strings.HasPrefix(r.URL.Path, "/api/orchestration/threads/"):
		th := f.threads[strings.TrimPrefix(r.URL.Path, "/api/orchestration/threads/")]
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
			tid, _ := cmd["threadId"].(string)
			title, _ := cmd["title"].(string)
			mode, _ := cmd["runtimeMode"].(string)
			f.threads[tid] = &thread{id: tid, title: title, runtime: mode}
			f.order = append(f.order, tid)
		case "thread.turn.start":
			tid, _ := cmd["threadId"].(string)
			th := f.threads[tid]
			if th == nil {
				writeJSON(w, 500, map[string]string{"error": "Thread does not exist"})
				return
			}
			msg, _ := cmd["message"].(map[string]any)
			th.messages = append(th.messages, map[string]any{"id": msg["messageId"], "role": "user", "text": msg["text"], "turnId": nil, "streaming": false})
			th.turns = append(th.turns, &turn{id: id(), state: "running", lag: f.LagPolls})
		}
		writeJSON(w, 200, map[string]any{"sequence": len(f.commands)})
	default:
		writeJSON(w, 404, map[string]string{"error": "not_found"})
	}
}

// snapshot — снимок треда; каждый опрос двигает идущий ход.
func (f *Fake) snapshot(th *thread) map[string]any {
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
			if f.FinishAfter > 0 && cur.polls >= f.FinishAfter {
				cur.state = f.FinalState
				th.messages = append(th.messages, map[string]any{"id": "assistant:" + cur.id, "role": "assistant",
					"text": f.Answer, "turnId": cur.id, "streaming": false})
			}
		}
		if cur.state == "error" {
			out["session"] = map[string]any{"status": "error", "lastError": "провайдер упал"}
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

// ── управление из теста ─────────────────────────────────────────────────

// NewCode — выдать одноразовый pairing-код (как `t3 auth pairing create`).
func (f *Fake) NewCode() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	code := "code-" + id()
	f.codes[code] = true
	return code
}

// AddCode — принять заранее известный код.
func (f *Fake) AddCode(code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes[code] = true
}

// AddToken — принять готовый bearer-токен (как token_file владельца).
func (f *Fake) AddToken(tok string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[tok] = true
}

// SetAdmin — токен с access:write для POST /api/auth/pairing-token.
func (f *Fake) SetAdmin(tok string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.admin = tok
}

// RevokeAll — отозвать все выданные токены (следующий запрос получит 401).
func (f *Fake) RevokeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.tokens)
}

// Issued — сколько токенов выдано обменом кода.
func (f *Fake) Issued() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issued
}

// Exchanges — формы запросов /oauth/token.
func (f *Fake) Exchanges() []map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]string(nil), f.exchanges...)
}

// Commands — команды dispatch данного типа ("" — все), по порядку.
func (f *Fake) Commands(kind string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, c := range f.commands {
		if kind == "" || c["type"] == kind {
			out = append(out, c)
		}
	}
	return out
}

// Last — последняя команда типа kind (nil — не было).
func (f *Fake) Last(kind string) map[string]any {
	cmds := f.Commands(kind)
	if len(cmds) == 0 {
		return nil
	}
	return cmds[len(cmds)-1]
}

// Archive — убрать тред в архив.
func (f *Fake) Archive(threadID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if th := f.threads[threadID]; th != nil {
		th.archived = true
	}
}

// Delete — удалить тред (снимок ответит 404).
func (f *Fake) Delete(threadID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.threads, threadID)
}

// Title — заголовок треда ("" — нет такого).
func (f *Fake) Title(threadID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if th := f.threads[threadID]; th != nil {
		return th.title
	}
	return ""
}

// Running — идёт ли в треде ход.
func (f *Fake) Running(threadID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	th := f.threads[threadID]
	return th != nil && len(th.turns) > 0 && th.turns[len(th.turns)-1].state == "running"
}

// Finish — закончить идущий ход сейчас же.
func (f *Fake) Finish(threadID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	th := f.threads[threadID]
	if th == nil || len(th.turns) == 0 {
		return
	}
	cur := th.turns[len(th.turns)-1]
	if cur.state == "running" {
		cur.state, cur.lag = f.FinalState, 0
		th.messages = append(th.messages, map[string]any{"id": "assistant:" + cur.id, "role": "assistant",
			"text": f.Answer, "turnId": cur.id, "streaming": false})
	}
}
