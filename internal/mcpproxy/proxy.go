// Package mcpproxy — `tg mcp`: MCP-сервер агента как тонкая прослойка.
//
// MCP-сервер живёт, пока открыта сессия Claude Code, — часами, и его код
// обновляется только с новой сессией. Поэтому логики здесь нет: вызовы
// инструментов уходят в службу, а если её нет — в свежий bin\tg.exe
// (`tg tool-call`). Когда сменилась сборка службы или exe, прослойка
// перечитывает список инструментов, и клиент получает tools/list_changed.
package mcpproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"tgagent/internal/config"
	"tgagent/internal/mcpserver"
	"tgagent/internal/service"
	"tgagent/internal/svc"
)

// currentExe — тот bin\tg.exe, что лежит в установке сейчас.
func currentExe() string {
	exe := filepath.Join(config.Root, "bin", "tg.exe")
	if _, err := os.Stat(exe); err == nil {
		return exe
	}
	self, _ := os.Executable()
	return self
}

func child(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	exe := currentExe()
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
	build  string    // сборка службы, с которой взят список
	stamp  time.Time // время exe, с которого взят список (служба не запущена)
}

// tools — список инструментов: у службы, а без неё — у свежего exe.
func (p *proxy) tools(ctx context.Context) ([]*mcp.Tool, string, error) {
	var tools []*mcp.Tool
	build, err := svc.Call(ctx, "tools", nil, &tools)
	if err == nil {
		return tools, build, nil
	}
	if !errors.Is(err, svc.Unavailable) {
		return nil, "", err
	}
	raw, err := child(ctx, nil, "tool-list")
	if err != nil {
		return nil, "", err
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, "", fmt.Errorf("tool-list: %w", err)
	}
	return tools, "", nil
}

func (p *proxy) load(ctx context.Context) error {
	tools, build, err := p.tools(ctx)
	if err != nil {
		return err
	}
	var stamp time.Time
	if st, err := os.Stat(currentExe()); err == nil {
		stamp = st.ModTime()
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
	p.names, p.build, p.stamp = fresh, build, stamp
	return nil
}

func (p *proxy) handler(name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := json.RawMessage(req.Params.Arguments)
		var res mcp.CallToolResult
		build, err := svc.Call(ctx, "tool", service.ToolArgs{Name: name, Args: args}, &res)
		if err == nil {
			p.noticeBuild(build)
			return &res, nil
		}
		if !errors.Is(err, svc.Unavailable) {
			return mcpserver.Failed(err), nil
		}
		// службы нет — свежий exe на месте
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		raw, err := child(ctx, args, "tool-call", name)
		if err != nil {
			return mcpserver.Failed(err), nil
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return mcpserver.Failed(fmt.Errorf("ответ tool-call не разобрался: %w", err)), nil
		}
		return &res, nil
	}
}

// noticeBuild — служба ответила другой сборкой: перечитать инструменты.
func (p *proxy) noticeBuild(build string) {
	p.mu.Lock()
	changed := build != "" && build != p.build
	p.mu.Unlock()
	if changed {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = p.load(ctx)
		}()
	}
}

// watch — без службы сборку видно по exe; со службой — по её ответам
// (и тут, раз в минуту, если агент молчит).
func (p *proxy) watch(ctx context.Context) {
	tick := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		tick++
		st, err := os.Stat(currentExe())
		p.mu.Lock()
		exeChanged := err == nil && !st.ModTime().Equal(p.stamp)
		p.mu.Unlock()
		if exeChanged || tick%12 == 0 {
			var status service.Status
			if build, err := svc.Call(ctx, "status", nil, &status); err == nil {
				p.noticeBuild(build)
				if !exeChanged {
					continue
				}
			}
			if exeChanged {
				_ = p.load(ctx)
			}
		}
	}
}

// Run — обслуживать MCP по stdio.
func Run(ctx context.Context) error {
	p := &proxy{
		server: mcp.NewServer(&mcp.Implementation{Name: "telegram", Version: "0.3.0"},
			&mcp.ServerOptions{Instructions: mcpserver.Instructions()}),
		names: map[string]bool{},
	}
	if err := p.load(ctx); err != nil {
		// ни службы, ни свежего exe — работаем своим кодом, чем ничем
		fmt.Fprintln(os.Stderr, "mcp: прослойка не поднялась, работаю напрямую:", err)
		return mcpserver.Run(ctx)
	}
	go p.watch(ctx)
	return p.server.Run(ctx, &mcp.StdioTransport{})
}
