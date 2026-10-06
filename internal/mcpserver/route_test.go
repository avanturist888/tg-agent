package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"tgagent/internal/config"
	"tgagent/internal/core"
	"tgagent/internal/inbox"
	"tgagent/internal/svc"
)

// fakeAccount — служба аккаунта work: на «tool» отвечает тем, что получила.
type fakeAccount struct {
	mu    sync.Mutex
	calls []forwardArgs
}

func (f *fakeAccount) got() []forwardArgs {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forwardArgs(nil), f.calls...)
}

// routeEnv — установка с аккаунтами main и work; служба work — подделка на
// её настоящем канале.
func routeEnv(t *testing.T) *fakeAccount {
	t.Helper()
	old := config.Root
	config.Root = t.TempDir()
	t.Cleanup(func() { config.Root = old; _ = config.SetProfile("") })
	write := func(rel, body string) {
		path := filepath.Join(config.Root, rel)
		os.MkdirAll(filepath.Dir(path), 0o700)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".env", "TG_API_ID=1\nTG_API_HASH=x\nTG_SEND_POLICY=human_approval\nTG_BOT_ENV_FILE=nope\n")
	write("config/chats.toml", "[[chat]]\nalias = \"team\"\nid = -100123\nsend = true\n")
	write("data/account.json", `{"first_name":"Основной"}`)
	write("profiles/work/.env", "TG_API_ID=1\nTG_API_HASH=x\nTG_SEND_POLICY=human_approval\n")
	write("profiles/work/config/chats.toml",
		"[[chat]]\nalias = \"team\"\nid = -100555\n\n[[chat]]\nalias = \"client\"\nid = -100777\n")

	f := &fakeAccount{}
	tool := func(ctx context.Context, raw json.RawMessage) (any, error) {
		var a forwardArgs
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.calls = append(f.calls, a)
		f.mu.Unlock()
		body := `{"account":"work","from":"work","tool":"` + a.Name + `"}`
		if a.Name == "tg_wait_approval" {
			body = `{"draft_id":"x","status":"waiting"}`
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: body}}}, nil
	}
	if err := config.SetProfile("work"); err != nil {
		t.Fatal(err)
	}
	srv, err := svc.Listen(map[string]svc.Handler{"tool": tool}, ErrorKind)
	_ = config.SetProfile("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx)
	t.Cleanup(cancel)
	return f
}

func callText(t *testing.T, ctx context.Context, name, args string) string {
	t.Helper()
	res, err := CallTool(ctx, name, json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	return res.Content[0].(*mcp.TextContent).Text
}

func TestRouteToOtherAccount(t *testing.T) {
	f := routeEnv(t)
	agent := &inbox.Addr{Session: "s1", Socket: "pipe", Token: "tok"}
	ctx := core.WithAgent(context.Background(), agent)

	// ссылка с аккаунтом — в его службу, с адресом сессии и пометкой Local
	if out := callText(t, ctx, "tg_read_chat", `{"chat":"work/team","limit":5}`); !strings.Contains(out, `"from":"work"`) {
		t.Fatalf("work/team: %s", out)
	}
	// alias, который есть только у work, — тоже туда
	callText(t, ctx, "tg_subscribe", `{"chat":"client"}`)
	calls := f.got()
	if len(calls) != 2 || calls[0].Name != "tg_read_chat" || !calls[0].Local || calls[0].Agent == nil ||
		calls[0].Agent.Session != "s1" || !strings.Contains(string(calls[0].Args), "work/team") ||
		calls[1].Name != "tg_subscribe" {
		t.Fatalf("передача: %+v", calls)
	}
	// alias, который есть у обоих, — свой
	if acct, err := chatAccount("team"); err != nil || acct != "main" {
		t.Fatalf("team: %s %v", acct, err)
	}
	if acct, err := chatAccount("-100555"); err != nil || acct != "work" {
		t.Fatalf("по id: %s %v", acct, err)
	}

	// папки чужого аккаунта — по account; неизвестный аккаунт — ошибка
	callText(t, ctx, "tg_list_folders", `{"account":"work"}`)
	if out := callText(t, ctx, "tg_list_folders", `{"account":"nobody"}`); !strings.Contains(out, "Аккаунта 'nobody' нет") {
		t.Fatalf("неизвестный аккаунт: %s", out)
	}

	// черновик не наш — спрашиваем остальных
	if out := callText(t, ctx, "tg_wait_approval", `{"draft_id":"0101-0000-abcd","timeout_sec":1}`); !strings.Contains(out, `"waiting"`) {
		t.Fatalf("черновик другого аккаунта: %s", out)
	}

	// список чатов — по аккаунтам
	out := callText(t, ctx, "tg_list_chats", `{}`)
	var list struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list.Accounts) != 2 ||
		list.Accounts[0]["account"] != "main" || list.Accounts[0]["user"] != "Основной" ||
		list.Accounts[1]["account"] != "work" || list.Accounts[1]["tool"] != "tg_list_chats" {
		t.Fatalf("tg_list_chats по аккаунтам: %s", out)
	}

	// вызов, уже переданный из другой службы, дальше не идёт
	before := len(f.got())
	callText(t, WithoutRouting(ctx), "tg_list_folders", `{"account":"work"}`)
	// агент из контейнера — только свой аккаунт
	callText(t, WithAgent(context.Background(), &config.Agent{Name: "box", Chats: []string{"team"}}),
		"tg_read_chat", `{"chat":"work/team"}`)
	if len(f.got()) != before {
		t.Fatalf("лишняя передача: %+v", f.got()[before:])
	}
}
