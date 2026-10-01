package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"tgagent/internal/config"
)

const agentToken = "test-agent-token-0123456789abcdef"

// httpEnv — установка во временной папке: два чата в белом списке, агент
// «box» с доступом только к test.
func httpEnv(t *testing.T) *httptest.Server {
	t.Helper()
	old := config.Root
	config.Root = t.TempDir()
	t.Cleanup(func() { config.Root = old })
	os.MkdirAll(filepath.Join(config.Root, "config"), 0o700)
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(config.Root, rel), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".env", "TG_API_ID=1\nTG_API_HASH=x\nTG_SEND_POLICY=human_approval\nTG_BOT_ENV_FILE=nope\n"+
		"TG_AGENT_BOX_TOKEN="+agentToken+"\nTG_AGENT_SHORT_TOKEN=short\n")
	write("config/chats.toml", `
[[chat]]
alias = "test"
id = -100123
title = "Тест"
send = true

[[chat]]
alias = "other"
id = -100456
title = "Чужой"
`)
	write("config/agents.toml", `
[[agent]]
name = "box"
token_env = "TG_AGENT_BOX_TOKEN"
chats = ["test"]

[[agent]]
name = "short"
token_env = "TG_AGENT_SHORT_TOKEN"
chats = ["test", "other"]
`)
	srv := httptest.NewServer(NewHTTP(config.LoadAgents, func() string {
		return filepath.Join(config.Root, "data", "audit.jsonl")
	}))
	t.Cleanup(srv.Close)
	return srv
}

type bearer struct{ tok string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, url, tok string) (*mcp.ClientSession, error) {
	t.Helper()
	tr := &mcp.StreamableClientTransport{Endpoint: url + HTTPPath, HTTPClient: &http.Client{Transport: bearer{tok}},
		DisableStandaloneSSE: true, MaxRetries: -1}
	return mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), tr, nil)
}

func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

func TestHTTPRejectsWithoutToken(t *testing.T) {
	srv := httpEnv(t)
	for _, tok := range []string{"", "wrong-token-wrong-token-wrong-token", "short"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+HTTPPath, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("токен %q: ждали 401, получили %d", tok, resp.StatusCode)
		}
	}
	if _, err := connect(t, srv.URL, "wrong-token-wrong-token-wrong-token"); err == nil {
		t.Fatal("MCP-клиент с чужим токеном подключился")
	}
	resp, err := http.Get(srv.URL + "/other")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("чужой путь: %d", resp.StatusCode)
	}
	raw, _ := os.ReadFile(filepath.Join(config.Root, "data", "audit.jsonl"))
	if !strings.Contains(string(raw), "mcp_http_denied") {
		t.Fatal("отказ должен попасть в аудит")
	}
}

func TestHTTPAgentScope(t *testing.T) {
	srv := httpEnv(t)
	cs, err := connect(t, srv.URL, agentToken)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	ctx := context.Background()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	if !names["tg_read_chat"] || !names["tg_draft_message"] || names["tg_download_file"] {
		t.Fatalf("инструменты агента из контейнера: %v", names)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "tg_list_chats", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(res); !strings.Contains(text, `"test"`) || strings.Contains(text, "other") || strings.Contains(text, "Чужой") {
		t.Fatalf("агенту видны только его чаты:\n%s", text)
	}

	// чат из общего белого списка, но не из списка агента — для него не существует
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "tg_read_chat", Arguments: map[string]any{"chat": "other"}})
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(res); !strings.Contains(text, "denied") || !strings.Contains(text, "не в белом списке") {
		t.Fatalf("чужой чат:\n%s", text)
	}

	raw, _ := os.ReadFile(filepath.Join(config.Root, "data", "audit.jsonl"))
	audit := string(raw)
	if !strings.Contains(audit, `"mcp_http_call"`) || !strings.Contains(audit, `"agent":"box"`) ||
		!strings.Contains(audit, `"tool":"tg_read_chat"`) || !strings.Contains(audit, `"result":"error"`) {
		t.Fatalf("аудит вызовов:\n%s", audit)
	}
}

func TestHTTPTokenChangeAppliesAtOnce(t *testing.T) {
	srv := httpEnv(t)
	if _, err := connect(t, srv.URL, agentToken); err != nil {
		t.Fatal(err)
	}
	// владелец сменил токен в .env — старый больше не пускает, без перезапуска
	env := filepath.Join(config.Root, ".env")
	raw, _ := os.ReadFile(env)
	os.WriteFile(env, []byte(strings.ReplaceAll(string(raw), agentToken, "new-agent-token-0123456789abcdef")), 0o600)
	if _, err := connect(t, srv.URL, agentToken); err == nil {
		t.Fatal("старый токен после смены всё ещё пускает")
	}
	if _, err := connect(t, srv.URL, "new-agent-token-0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
}
