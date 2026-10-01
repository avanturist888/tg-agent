package service

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"tgagent/internal/audit"
	"tgagent/internal/config"
	"tgagent/internal/mcpserver"
)

// MCP по HTTP для агентов из контейнеров (mcpserver/http.go). Слушает, только
// когда в config/agents.toml есть агенты; файл перечитывается раз в
// httpCheckEvery, так что включение, выключение и смена адреса не требуют
// перезапуска службы.

const httpCheckEvery = 30 * time.Second

var httpAddr struct {
	sync.Mutex
	addr string // где слушаем сейчас ("" — выключено)
}

// HTTPAddr — адрес MCP по HTTP, если он включён.
func HTTPAddr() string {
	httpAddr.Lock()
	defer httpAddr.Unlock()
	return httpAddr.addr
}

func runHTTP(ctx context.Context, loader func() (*config.Settings, error)) {
	auditPath := func() string {
		if s, err := loader(); err == nil {
			return s.AuditPath()
		}
		return (&config.Settings{}).AuditPath()
	}
	handler := mcpserver.NewHTTP(config.LoadAgents, auditPath)
	var (
		srv      *http.Server
		cur      string
		lastFail string
	)
	stop := func() {
		if srv == nil {
			return
		}
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = srv.Shutdown(sctx)
		cancel()
		_ = audit.Log(auditPath(), "mcp_http_stop", "listen", cur)
		srv, cur = nil, ""
		httpAddr.Lock()
		httpAddr.addr = ""
		httpAddr.Unlock()
	}
	check := func() {
		cfg, err := config.LoadAgents()
		if err != nil {
			if msg := err.Error(); msg != lastFail {
				lastFail = msg
				_ = audit.Log(auditPath(), "mcp_http_failed", "error", msg)
			}
			return // файл сломали — работаем как было, пока не починят
		}
		want := ""
		if len(cfg.List) > 0 {
			want = cfg.Listen
		}
		if want == cur {
			return
		}
		stop()
		if want == "" {
			return
		}
		ln, err := net.Listen("tcp", want)
		if err != nil {
			if msg := err.Error(); msg != lastFail {
				lastFail = msg
				_ = audit.Log(auditPath(), "mcp_http_failed", "listen", want, "error", msg)
			}
			return
		}
		lastFail = ""
		srv = &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
		cur = want
		httpAddr.Lock()
		httpAddr.addr = want
		httpAddr.Unlock()
		names := make([]string, 0, len(cfg.List))
		for _, a := range cfg.List {
			names = append(names, a.Name)
		}
		_ = audit.Log(auditPath(), "mcp_http_listen", "listen", want, "agents", strings.Join(names, ", "))
		go func(s *http.Server) {
			if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				_ = audit.Log(auditPath(), "mcp_http_failed", "listen", want, "error", err.Error())
			}
		}(srv)
	}
	check()
	tick := time.NewTicker(httpCheckEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			stop()
			return
		case <-tick.C:
			check()
		}
	}
}
