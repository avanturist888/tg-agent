//go:build live

package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/t3"
)

// go test -tags live -run LiveHTTP ./internal/mcpserver — агент в контейнере
// (TG_LIVE_CONTAINER, по умолчанию t3-bala) ходит к инструментам по HTTP.
// Сервер поднимается в тесте на свободном порту с разовым токеном и одним
// чатом saved; Claude Code в контейнере получает его через `claude mcp add`
// (после теста запись удаляется), затем ход в треде T3 «TG: проверка MCP по
// HTTP» вызывает tg_list_chats. Telegram не трогается.
func TestLiveHTTPFromContainer(t *testing.T) {
	container := os.Getenv("TG_LIVE_CONTAINER")
	if container == "" {
		container = "t3-bala"
	}
	if exec.Command("docker", "exec", container, "true").Run() != nil {
		t.Skip("контейнер " + container + " не запущен")
	}
	b := make([]byte, 32)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	os.Setenv("TG_LIVE_AGENT_TOKEN", tok)
	agents := func() (*config.Agents, error) {
		return &config.Agents{List: []config.Agent{{Name: "live-test", TokenEnv: "TG_LIVE_AGENT_TOKEN", Chats: []string{"saved"}}}}, nil
	}
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: NewHTTP(agents, func() string { return auditPath })}
	go srv.Serve(ln)
	defer srv.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	url := fmt.Sprintf("http://host.docker.internal:%d%s", port, HTTPPath)
	t.Logf("MCP по HTTP: %s", url)

	mask := func(s string) string { return strings.ReplaceAll(s, tok, "***") }
	docker := func(args ...string) (string, error) {
		out, err := exec.Command("docker", append([]string{"exec", container}, args...)...).CombinedOutput()
		return mask(string(out)), err
	}
	if out, err := docker("claude", "mcp", "add", "--scope", "user", "--transport", "http", "tg-live", url,
		"--header", "Authorization: Bearer "+tok); err != nil {
		t.Fatalf("claude mcp add: %v %s", err, out)
	}
	defer func() {
		out, err := docker("claude", "mcp", "remove", "--scope", "user", "tg-live")
		t.Logf("claude mcp remove: %v %s", err, strings.TrimSpace(out))
	}()
	out, _ := docker("claude", "mcp", "list")
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "tg-live") {
			t.Logf("claude mcp list: %s", strings.TrimSpace(line))
			if !strings.Contains(line, "Connected") {
				t.Fatalf("Claude Code в контейнере не подключился к MCP по HTTP")
			}
		}
	}

	e := &t3.Env{Name: "live", Origin: "http://127.0.0.1:3774", TokenPath: filepath.Join(t.TempDir(), "t3-token.json"),
		TokenFile: os.Getenv("T3_LIVE_TOKEN_FILE"), Model: "claude-sonnet-5-5", TurnTimeout: 4 * time.Minute,
		PairingCmd: []string{"docker", "exec", container, "t3", "auth", "pairing", "create", "--ttl", "5m", "--label", "tg-agent", "--json"}}
	ctx := context.Background()
	thread, err := e.CreateThread(ctx, "TG: проверка MCP по HTTP")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("тред %s", thread)
	prev, err := e.StartTurn(ctx, thread, "Вызови инструмент tg_list_chats MCP-сервера tg-live и ответь только "+
		"списком alias из его ответа через запятую, без других слов. Если инструмента нет — ответь НЕТ ИНСТРУМЕНТА.")
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.WaitTurn(ctx, thread, prev)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ход %s: %s, ответ %q", res.TurnID, res.State, res.Text)
	if res.State != "completed" || !strings.Contains(res.Text, "saved") {
		t.Fatalf("агент не увидел чат saved через MCP по HTTP: %+v", res)
	}
	raw, _ := os.ReadFile(auditPath)
	if !strings.Contains(string(raw), `"tool":"tg_list_chats"`) || !strings.Contains(string(raw), `"agent":"live-test"`) {
		t.Fatalf("аудит вызовов:\n%s", raw)
	}
	t.Logf("аудит: %d строк(и) mcp_http_call", strings.Count(string(raw), "mcp_http_call"))
}
