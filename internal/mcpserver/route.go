package mcpserver

// Несколько аккаунтов (профилей) за одним MCP-сервером.
//
// Профиль — глобальное состояние процесса, и у каждого аккаунта своя служба
// со своим соединением с Telegram. Поэтому сервер любого аккаунта сам
// выполняет только вызовы к своим чатам, а остальные передаёт службе нужного
// аккаунта (accounts.Call) с пометкой Local — там их уже не маршрутизируют:
//
//   - инструменты с chat — по ссылке «<аккаунт>/<alias>», а alias без
//     аккаунта ищется сначала у себя, потом у остальных (если он только у
//     одного из них);
//   - инструменты с account (папки, запросы доступа) — по нему;
//   - черновики и запросы доступа по id — сначала у себя, при not_found — у
//     остальных: id уникальны и так;
//   - tg_list_chats, tg_list_drafts и tg_unsubscribe без чата — у всех, ответ
//     по аккаунтам.
//
// Пока аккаунт один, маршрутизации нет вовсе. Агентам из контейнеров (HTTP)
// она не положена: у них свой урезанный список одного аккаунта.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"tgagent/internal/accounts"
	"tgagent/internal/config"
	"tgagent/internal/core"
	"tgagent/internal/inbox"
	"tgagent/internal/omap"
)

type noRouteKey struct{}

// WithoutRouting — вызов уже передан из службы другого аккаунта: выполнять у себя.
func WithoutRouting(ctx context.Context) context.Context {
	return context.WithValue(ctx, noRouteKey{}, true)
}

func routable(ctx context.Context) bool {
	return ctx.Value(noRouteKey{}) == nil && agentOf(ctx) == nil
}

// forwardArgs — то же, что service.ToolArgs (service импортирует этот пакет).
type forwardArgs struct {
	Name  string          `json:"name"`
	Args  json.RawMessage `json:"args,omitempty"`
	Agent *inbox.Addr     `json:"agent,omitempty"`
	Local bool            `json:"local,omitempty"`
}

var (
	chatTools = set("tg_read_chat", "tg_view_media", "tg_download_file", "tg_transcribe", "tg_search_chat",
		"tg_list_topics", "tg_draft_message", "tg_react", "tg_subscribe", "tg_unsubscribe")
	accountTools = set("tg_list_folders", "tg_folder_chats", "tg_request_access")
	idTools      = set("tg_wait_approval", "tg_send_draft", "tg_cancel_draft", "tg_wait_access")
	everyTools   = set("tg_list_chats", "tg_list_drafts")
)

func set(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

// route — промежуточный слой MCP: вызов к чужому аккаунту уходит в его службу.
func route(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		call, ok := req.(*mcp.CallToolRequest)
		if method != "tools/call" || !ok || call.Params == nil || !routable(ctx) || !config.Multi() {
			return next(ctx, method, req)
		}
		name, raw := call.Params.Name, call.Params.Arguments
		var args map[string]any
		_ = json.Unmarshal(raw, &args)
		arg := func(key string) string {
			v, _ := args[key].(string)
			return strings.TrimSpace(v)
		}
		local := func() (mcp.Result, error) { return next(ctx, method, req) }

		switch {
		case chatTools[name] && !(name == "tg_unsubscribe" && arg("chat") == ""):
			acct, err := chatAccount(arg("chat"))
			if err != nil {
				return Failed(err), nil
			}
			if config.IsOwnAccount(acct) {
				return local()
			}
			return forward(ctx, acct, name, raw), nil

		case accountTools[name]:
			if config.IsOwnAccount(arg("account")) {
				return local()
			}
			acct, err := knownAccount(arg("account"))
			if err != nil {
				return Failed(err), nil
			}
			return forward(ctx, acct, name, raw), nil

		case idTools[name]:
			res, err := local()
			if err != nil || !notFound(res) {
				return res, err
			}
			for _, acct := range others() {
				if r := forward(ctx, acct, name, raw); !notFound(r) {
					return r, nil
				}
			}
			return res, nil

		case everyTools[name] || name == "tg_unsubscribe":
			res, err := local()
			if err != nil {
				return nil, err
			}
			parts := []*omap.Map{part(config.AccountName(), res)}
			for _, acct := range others() {
				parts = append(parts, part(acct, forward(ctx, acct, name, raw)))
			}
			return text(omap.Pretty(omap.New().Set("accounts", parts).Set("hint",
				"Аккаунтов несколько. К чату не основного аккаунта обращайся «<аккаунт>/<alias>» "+
					"(alias в списке уже так и записан); alias, который есть только у одного аккаунта, "+
					"работает и без него."))), nil
		}
		return local()
	}
}

// chatAccount — чей чат: «work/team» — work; alias без аккаунта — свой, если
// он есть в своём списке, иначе тот единственный аккаунт, у кого он есть.
func chatAccount(chat string) (string, error) {
	if acct, _, ok := config.SplitRef(chat); ok {
		return acct, nil
	}
	own := config.AccountName()
	if rules, order, err := config.LoadChats(config.ChatsFile()); err == nil {
		if _, ok := config.FindChat(rules, order, chat); ok {
			return own, nil
		}
	}
	var found []string
	for _, acct := range others() {
		rules, order, err := config.LoadChats(config.ChatsFileOf(acct))
		if err != nil {
			continue
		}
		if _, ok := config.FindChat(rules, order, chat); ok {
			found = append(found, acct)
		}
	}
	switch len(found) {
	case 0:
		return own, nil // пусть свой список и откажет — с перечнем алиасов
	case 1:
		return found[0], nil
	}
	refs := make([]string, len(found))
	for i, a := range found {
		refs[i] = a + "/" + chat
	}
	return "", &core.Bad{Msg: fmt.Sprintf("Чат '%s' есть у нескольких аккаунтов: %s — укажи нужный.",
		chat, strings.Join(refs, ", "))}
}

// knownAccount — аккаунт по имени из аргумента account.
func knownAccount(name string) (string, error) {
	if acct := config.FindAccount(name); acct != "" {
		return acct, nil
	}
	return "", &core.Bad{Msg: fmt.Sprintf("Аккаунта '%s' нет. Аккаунты: %s.", name,
		strings.Join(config.Accounts(), ", "))}
}

// checkAccount — инструмент с account выполняется только у своего аккаунта
// (к чужому вызов передаёт route).
func checkAccount(name string) error {
	if config.IsOwnAccount(name) {
		return nil
	}
	if _, err := knownAccount(name); err != nil {
		return err
	}
	return &core.Bad{Msg: fmt.Sprintf("Аккаунт '%s' обслуживает своя служба, а вызов к нему сюда пришёл "+
		"без передачи — повтори через основной MCP-сервер telegram.", name)}
}

func others() []string {
	var out []string
	for _, a := range config.Accounts() {
		if !config.IsOwnAccount(a) {
			out = append(out, a)
		}
	}
	return out
}

// forward — вызвать инструмент в службе аккаунта acct.
func forward(ctx context.Context, acct, name string, raw json.RawMessage) *mcp.CallToolResult {
	var res mcp.CallToolResult
	err := accounts.Call(ctx, acct, "tool", forwardArgs{Name: name, Args: raw, Agent: core.AgentOf(ctx), Local: true}, &res)
	if err != nil {
		return Failed(fmt.Errorf("аккаунт %s: %w", acct, err))
	}
	return &res
}

// resultMap — ответ инструмента как объект (текст первого блока).
func resultMap(res mcp.Result) *omap.Map {
	r, ok := res.(*mcp.CallToolResult)
	if !ok || r == nil || len(r.Content) == 0 {
		return nil
	}
	t, ok := r.Content[0].(*mcp.TextContent)
	if !ok {
		return nil
	}
	m, err := omap.Decode([]byte(t.Text))
	if err != nil {
		return omap.New().Set("message", t.Text)
	}
	return m
}

func notFound(res mcp.Result) bool {
	m := resultMap(res)
	if m == nil {
		return false
	}
	for _, key := range []string{"error", "status"} {
		if v, ok := m.Get(key); ok {
			var s string
			if raw, ok := v.(json.RawMessage); ok && json.Unmarshal(raw, &s) == nil && s == "not_found" {
				return true
			}
		}
	}
	return false
}

// part — ответ одного аккаунта с его именем первым полем.
func part(acct string, res mcp.Result) *omap.Map {
	out := omap.New().Set("account", acct)
	m := resultMap(res)
	if m == nil {
		return out.Set("error", "error").Set("message", "пустой ответ")
	}
	m.Delete("account")
	m.Delete("hint")
	return out.Merge(m)
}
