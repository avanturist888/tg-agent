package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"tgagent/internal/config"
	"tgagent/internal/omap"
)

// MCP-сервер живёт, пока открыта сессия Claude Code, — часами. Чтобы новая
// сборка доходила до агентов без перезапуска их сессий, `tg mcp` — тонкая
// прослойка: каждый вызов инструмента она выполняет свежим bin\tg.exe
// (`tg tool-call`), а когда сборка меняется, перечитывает список
// инструментов и сообщает клиенту (notifications/tools/list_changed).
// Логики в прослойке нет — ей самой обновляться почти не нужно.

// currentExe — тот bin\tg.exe, что лежит в установке сейчас (сборка кладёт
// новый под тем же именем, а работающий переименовывает).
func currentExe() string {
	exe := filepath.Join(config.Root, "bin", "tg.exe")
	if _, err := os.Stat(exe); err == nil {
		return exe
	}
	self, _ := os.Executable()
	return self
}

func child(ctx context.Context, exe string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	hideWindow(cmd)
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.MultiWriter(&errBuf, os.Stderr)
	if err := cmd.Run(); err != nil {
		msg := bytes.TrimSpace(errBuf.Bytes())
		if len(msg) > 500 {
			msg = msg[len(msg)-500:]
		}
		return nil, fmt.Errorf("%s %v: %v %s", filepath.Base(exe), args, err, msg)
	}
	return out.Bytes(), nil
}

type proxy struct {
	server *mcp.Server
	mu     sync.Mutex
	names  map[string]bool
	stamp  time.Time
}

func (p *proxy) load(ctx context.Context) error {
	exe := currentExe()
	st, err := os.Stat(exe)
	if err != nil {
		return err
	}
	raw, err := child(ctx, exe, nil, "tool-list")
	if err != nil {
		return err
	}
	var tools []*mcp.Tool
	if err := json.Unmarshal(raw, &tools); err != nil {
		return fmt.Errorf("tool-list: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	fresh := map[string]bool{}
	for _, t := range tools {
		fresh[t.Name] = true
	}
	var gone []string
	for name := range p.names {
		if !fresh[name] {
			gone = append(gone, name)
		}
	}
	if len(gone) > 0 {
		p.server.RemoveTools(gone...)
	}
	for _, t := range tools {
		p.server.AddTool(t, p.handler(t.Name))
	}
	p.names, p.stamp = fresh, st.ModTime()
	return nil
}

func (p *proxy) handler(name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := []byte(req.Params.Arguments)
		if len(args) == 0 {
			args = []byte("{}")
		}
		raw, err := child(ctx, currentExe(), args, "tool-call", name)
		if err != nil {
			return failed(err), nil
		}
		var res mcp.CallToolResult
		if err := json.Unmarshal(raw, &res); err != nil {
			return failed(fmt.Errorf("ответ tool-call не разобрался: %w", err)), nil
		}
		return &res, nil
	}
}

func failed(err error) *mcp.CallToolResult {
	text := omap.Pretty(omap.New().Set("error", "internal").Set("message", err.Error()))
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// watch — сменилась сборка: перечитать инструменты (описания, новые поля).
func (p *proxy) watch(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		st, err := os.Stat(currentExe())
		if err != nil {
			continue
		}
		p.mu.Lock()
		changed := !st.ModTime().Equal(p.stamp)
		p.mu.Unlock()
		if changed {
			// файл мог ещё дописываться — перечитаем со следующей попытки, если не вышло
			_ = p.load(ctx)
		}
	}
}

// RunProxy — `tg mcp`: обслуживать MCP по stdio через свежую сборку.
func RunProxy(ctx context.Context) error {
	p := &proxy{
		server: mcp.NewServer(&mcp.Implementation{Name: "telegram", Version: "0.2.0"}, &mcp.ServerOptions{Instructions: instructions}),
		names:  map[string]bool{},
	}
	if err := p.load(ctx); err != nil {
		// свежая сборка не отвечает — работаем своим кодом, чем ничем
		fmt.Fprintln(os.Stderr, "mcp: прослойка не поднялась, работаю напрямую:", err)
		return Run(ctx)
	}
	go p.watch(ctx)
	return p.server.Run(ctx, &mcp.StdioTransport{})
}

// ── то, что выполняет свежая сборка ──────────────────────────────────────

func inMemory(ctx context.Context) (*mcp.ClientSession, func(), error) {
	ct, st := mcp.NewInMemoryTransports()
	ss, err := New().Connect(ctx, st, nil)
	if err != nil {
		return nil, nil, err
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "proxy", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		ss.Close()
		return nil, nil, err
	}
	return cs, func() { cs.Close(); ss.Close() }, nil
}

// ToolList — `tg tool-list`: описания инструментов этой сборки (JSON в stdout).
func ToolList(ctx context.Context, out io.Writer) error {
	cs, done, err := inMemory(ctx)
	if err != nil {
		return err
	}
	defer done()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(res.Tools)
}

// ToolCall — `tg tool-call <name>`: аргументы JSON из stdin, результат в stdout.
func ToolCall(ctx context.Context, name string, in io.Reader, out io.Writer) error {
	raw, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	var args map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("аргументы: %w", err)
		}
	}
	cs, done, err := inMemory(ctx)
	if err != nil {
		return err
	}
	defer done()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		res = failed(err)
	}
	return json.NewEncoder(out).Encode(res)
}
