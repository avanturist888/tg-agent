package mcpserver

// MCP по HTTP (streamable HTTP, без сессий) — для агентов в контейнерах:
// именованный канал ноутбука им не виден, а stdio-прослойку в контейнере не
// запустить. Каждый запрос несёт bearer-токен агента из config/agents.toml
// (сам токен — в .env); по нему служба узнаёт агента и урезает белый список
// до его чатов (config.Settings.Restrict). Инструменты те же, что по stdio,
// кроме tg_download_file: файл лёг бы на диск шлюза, а не к агенту.
//
// Транспорт живёт в службе (service/http.go). Без сессий: перезапуск службы
// агенту незаметен, отдельного состояния на агента нет.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"tgagent/internal/audit"
	"tgagent/internal/config"
)

// HTTPPath — путь MCP-сервера: http://<listen>/mcp.
const HTTPPath = "/mcp"

type agentKey struct{}

// WithAgent — вызов от агента из контейнера: права урезаются до его чатов.
func WithAgent(ctx context.Context, a *config.Agent) context.Context {
	return context.WithValue(ctx, agentKey{}, a)
}

func agentOf(ctx context.Context) *config.Agent {
	a, _ := ctx.Value(agentKey{}).(*config.Agent)
	return a
}

// load — настройки для вызова: у агента из контейнера — только его чаты.
func load(ctx context.Context) (*config.Settings, error) {
	s, err := config.Load()
	if err != nil {
		return nil, err
	}
	if a := agentOf(ctx); a != nil {
		s = s.Restrict(a)
	}
	return s, nil
}

// HTTP — обработчик MCP по HTTP.
type HTTP struct {
	agents    func() (*config.Agents, error)
	auditPath func() string
	handler   *mcp.StreamableHTTPHandler

	mu     sync.Mutex
	denied map[string]time.Time // адрес → когда последний раз писали отказ в аудит
}

type serverKey struct{}

// NewHTTP — обработчик; agents перечитывается на каждом запросе, так что
// правка agents.toml или токена в .env действует сразу.
func NewHTTP(agents func() (*config.Agents, error), auditPath func() string) *HTTP {
	h := &HTTP{agents: agents, auditPath: auditPath, denied: map[string]time.Time{}}
	h.handler = mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		srv, _ := r.Context().Value(serverKey{}).(*mcp.Server)
		return srv
	}, &mcp.StreamableHTTPOptions{
		Stateless: true,
		// из контейнера запрос приходит на loopback с Host host.docker.internal;
		// от DNS rebinding защищает bearer-токен, без него сюда не попасть
		DisableLocalhostProtection: true,
	})
	return h
}

func (h *HTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != HTTPPath {
		http.NotFound(w, r)
		return
	}
	a, why := h.authenticate(r)
	if a == nil {
		h.logDenied(r, why)
		w.Header().Set("WWW-Authenticate", `Bearer realm="tg-agent"`)
		http.Error(w, "tg-agent: "+why, http.StatusUnauthorized)
		return
	}
	ctx := context.WithValue(r.Context(), serverKey{}, h.serverFor(a))
	h.handler.ServeHTTP(w, r.WithContext(ctx))
}

// authenticate — агент по bearer-токену (nil и причина — чужой запрос).
func (h *HTTP) authenticate(r *http.Request) (*config.Agent, string) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	tok = strings.TrimSpace(tok)
	if !ok || tok == "" {
		return nil, "нужен заголовок Authorization: Bearer <токен агента>"
	}
	cfg, err := h.agents()
	if err != nil {
		return nil, "config/agents.toml не читается"
	}
	var found *config.Agent
	for i := range cfg.List {
		want := cfg.List[i].Token()
		if len(want) < config.MinTokenLen {
			continue // пустой или слишком короткий токен агента не принимаем
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(want)) == 1 {
			found = &cfg.List[i]
		}
	}
	if found == nil {
		return nil, "токен не принадлежит ни одному агенту из config/agents.toml"
	}
	return found, ""
}

func (h *HTTP) logDenied(r *http.Request, why string) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	h.mu.Lock()
	last := h.denied[host]
	if time.Since(last) < time.Minute {
		h.mu.Unlock()
		return // перебор токенов не должен раздувать аудит
	}
	h.denied[host] = time.Now()
	h.mu.Unlock()
	_ = audit.Log(h.auditPath(), "mcp_http_denied", "remote", host, "reason", why)
}

// serverFor — MCP-сервер для одного запроса агента a: те же инструменты, в
// контексте вызова — агент, каждый вызов — в аудит с его именем.
func (h *HTTP) serverFor(a *config.Agent) *mcp.Server {
	srv := New()
	srv.RemoveTools("tg_download_file")
	srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(WithAgent(ctx, a), method, req)
			if call, ok := req.(*mcp.CallToolRequest); ok && method == "tools/call" {
				h.logCall(a, call, res, err)
			}
			return res, err
		}
	})
	return srv
}

func (h *HTTP) logCall(a *config.Agent, call *mcp.CallToolRequest, res mcp.Result, err error) {
	var args struct {
		Chat    string `json:"chat"`
		DraftID string `json:"draft_id"`
	}
	if call.Params != nil {
		_ = json.Unmarshal(call.Params.Arguments, &args)
	}
	kv := []any{"agent", a.Name, "tool", call.Params.Name}
	if args.Chat != "" {
		kv = append(kv, "chat", args.Chat)
	}
	if args.DraftID != "" {
		kv = append(kv, "draft_id", args.DraftID)
	}
	switch r := res.(type) {
	case *mcp.CallToolResult:
		if r.IsError || (len(r.Content) == 1 && isErrorText(r.Content[0])) {
			kv = append(kv, "result", "error")
		}
	}
	if err != nil {
		kv = append(kv, "result", "error", "error", err.Error())
	}
	_ = audit.Log(h.auditPath(), "mcp_http_call", kv...)
}

// isErrorText — инструменты отдают ошибку текстом {"error": …, "message": …}.
func isErrorText(c mcp.Content) bool {
	t, ok := c.(*mcp.TextContent)
	if !ok {
		return false
	}
	var v struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	return json.Unmarshal([]byte(t.Text), &v) == nil && v.Error != "" && v.Message != ""
}
