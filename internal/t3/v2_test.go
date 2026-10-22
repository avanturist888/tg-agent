package t3

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// fakeHomeV2 — ~/.t3 десктопа на протоколе 2: сессия new-s в треде th-new,
// сессия до переезда old-s — в треде th-old (её текущая сессия — cur-s),
// arch-s — в треде из архива.
func fakeHomeV2(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("T3CODE_HOME", home)
	os.MkdirAll(filepath.Join(home, "userdata"), 0o700)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(home, "userdata", "statev2.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE orchestration_v2_projection_provider_threads (provider_thread_id TEXT PRIMARY KEY,
			thread_id TEXT, owner_node_id TEXT, updated_at TEXT NOT NULL, payload_json TEXT NOT NULL)`,
		`CREATE TABLE provider_session_runtime (thread_id TEXT PRIMARY KEY, last_seen_at TEXT NOT NULL,
			resume_cursor_json TEXT)`,
		`CREATE TABLE orchestration_v2_projection_threads (thread_id TEXT PRIMARY KEY, title TEXT, archived_at TEXT, deleted_at TEXT)`,
		`INSERT INTO orchestration_v2_projection_threads VALUES ('th-new', 'Новый', NULL, NULL), ('th-old', NULL, NULL, NULL),
			('th-arch', 'Архив', '2026-10-16T10:00:00Z', NULL)`,
		`INSERT INTO orchestration_v2_projection_provider_threads VALUES
			('p1', 'th-new', NULL, '2026-10-16T10:00:00Z', '{"nativeThreadRef":{"nativeId":"new-s"}}'),
			('p2', 'th-old', NULL, '2026-10-16T11:00:00Z', '{"nativeThreadRef":{"nativeId":"cur-s"}}'),
			('p3', 'th-arch', NULL, '2026-10-16T11:00:00Z', '{"nativeThreadRef":{"nativeId":"arch-s"}}')`,
		`INSERT INTO provider_session_runtime VALUES ('th-old', '2026-10-15T10:00:00Z', '{"resume":"old-s"}')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestThreadOfV2(t *testing.T) {
	fakeHomeV2(t)
	ctx := context.Background()
	for session, want := range map[string]string{"new-s": "th-new", "old-s": "th-old", "cur-s": "th-old"} {
		if got, err := ThreadOf(ctx, session); err != nil || got != want {
			t.Fatalf("%s: тред %q, %v; ждали %q", session, got, err, want)
		}
	}
	for _, session := range []string{"arch-s", "nobody"} {
		if _, err := ThreadOf(ctx, session); !errors.Is(err, NoThread) {
			t.Fatalf("%s: ждали NoThread, а не %v", session, err)
		}
	}
	if cur := CurrentSession(ctx, "th-old"); cur != "cur-s" {
		t.Fatalf("текущая сессия треда: %q", cur)
	}
	if th, err := ThreadInfo(ctx, "arch-s"); err != nil || th.ID != "th-arch" || th.Title != "Архив" || !th.Closed {
		t.Fatalf("закрытый тред с заголовком: %+v %v", th, err)
	}
	if th, err := ThreadInfo(ctx, "new-s"); err != nil || th.Title != "Новый" || th.Closed {
		t.Fatalf("открытый тред: %+v %v", th, err)
	}
}

// fakeWS — сервер T3 v2: тикет по токену и RPC по /ws; reply — ответ на команду.
func fakeWS(t *testing.T, reply func(cmd map[string]any) string) (*Env, *[]map[string]any) {
	t.Helper()
	var got []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/websocket-ticket", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"ticket": "tk-1", "expiresAt": "2026-10-16T12:00:00Z"})
	})
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("orchestrationProtocol") != "2" || r.URL.Query().Get("wsTicket") != "tk-1" {
			http.Error(w, "bad", http.StatusUpgradeRequired)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		_, raw, err := c.Read(r.Context())
		if err != nil {
			return
		}
		var req struct {
			Tag     string         `json:"_tag"`
			ID      string         `json:"id"`
			Method  string         `json:"tag"`
			Payload map[string]any `json:"payload"`
		}
		json.Unmarshal(raw, &req)
		if req.Tag != "Request" || req.Method != "orchestration.dispatchCommand" {
			t.Errorf("запрос RPC: %s", raw)
			return
		}
		got = append(got, req.Payload)
		c.Write(r.Context(), websocket.MessageText, []byte(`{"_tag":"Pong"}`))
		c.Write(r.Context(), websocket.MessageText, []byte(reply(req.Payload)))
		c.Read(r.Context()) // ждём, пока клиент закроет
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	tf := filepath.Join(t.TempDir(), "token.json")
	os.WriteFile(tf, []byte(`{"token":"tok"}`), 0o600)
	return &Env{Name: "fake", Origin: srv.URL, TokenFile: tf, TokenPath: filepath.Join(t.TempDir(), "t.json")}, &got
}

func TestSendV2(t *testing.T) {
	e, got := fakeWS(t, func(map[string]any) string {
		return `[{"_tag":"Exit","requestId":"1","exit":{"_tag":"Success","value":{"sequence":5}}}]`
	})
	if err := e.sendV2(context.Background(), "th-1", "привет"); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 {
		t.Fatalf("команд: %d", len(*got))
	}
	cmd := (*got)[0]
	mode, _ := cmd["dispatchMode"].(map[string]any)
	if cmd["type"] != "message.dispatch" || cmd["threadId"] != "th-1" || cmd["text"] != "привет" ||
		cmd["deliveryIntent"] != "steer" || mode["type"] != "start_immediately" || cmd["commandId"] == "" || cmd["messageId"] == "" {
		t.Fatalf("команда: %v", cmd)
	}
}

func TestSendV2Failures(t *testing.T) {
	e, _ := fakeWS(t, func(map[string]any) string {
		return `{"_tag":"Exit","requestId":"1","exit":{"_tag":"Failure","cause":[{"_tag":"Fail","error":{"_tag":"OrchestrationV2DispatchCommandError","message":"No orchestration projection exists for thread th-1.","cause":{"name":"OrchestratorProjectionError","cause":{"name":"ProjectionStoreThreadNotFoundError"}}}}]}}`
	})
	if err := e.sendV2(context.Background(), "th-1", "x"); !errors.Is(err, ThreadGone) {
		t.Fatalf("ждали ThreadGone, а не %v", err)
	}
	e, _ = fakeWS(t, func(map[string]any) string {
		return `{"_tag":"Exit","requestId":"1","exit":{"_tag":"Failure","cause":[{"_tag":"Die","defect":"Missing key at [\"createdBy\"]"}]}}`
	})
	if err := e.sendV2(context.Background(), "th-1", "x"); err == nil || !strings.Contains(err.Error(), "createdBy") {
		t.Fatalf("ждали отказ с причиной, а не %v", err)
	}
}
