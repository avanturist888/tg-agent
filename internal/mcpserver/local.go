package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"tgagent/internal/omap"
)

// Инструменты можно вызывать и без stdio: служба и `tg tool-call` поднимают
// сервер в памяти и ходят в него как клиент — со всей проверкой аргументов
// по схеме, как у настоящего MCP-клиента.

func inMemory(ctx context.Context) (*mcp.ClientSession, func(), error) {
	ct, st := mcp.NewInMemoryTransports()
	ss, err := New().Connect(ctx, st, nil)
	if err != nil {
		return nil, nil, err
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "local", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		ss.Close()
		return nil, nil, err
	}
	return cs, func() { cs.Close(); ss.Close() }, nil
}

// Tools — описания инструментов этой сборки.
func Tools(ctx context.Context) ([]*mcp.Tool, error) {
	cs, done, err := inMemory(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	return res.Tools, nil
}

// CallTool — вызвать инструмент этой сборки.
func CallTool(ctx context.Context, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	var parsed map[string]any
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &parsed); err != nil {
			return nil, fmt.Errorf("аргументы %s: %w", name, err)
		}
	}
	cs, done, err := inMemory(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: parsed})
	if err != nil {
		return Failed(err), nil
	}
	return res, nil
}

// Failed — результат-ошибка в том же виде, что у инструментов.
func Failed(err error) *mcp.CallToolResult {
	text := omap.Pretty(omap.New().Set("error", ErrorKind(err)).Set("message", err.Error()))
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// Instructions — текст для клиента при подключении.
func Instructions() string { return instructions }
