package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/core"
	"tgagent/internal/omap"
	"tgagent/internal/service"
)

const t3Usage = `Окружения T3 Code (config/t3.toml) и треды шлюза в них:
  tg t3                              окружения и треды шлюза (без сети)
  tg t3 check [окружение]            сервер, протокол, токен, проект, живы ли треды
  tg t3 subscribe <окр> <alias> [--after N]   будить тред шлюза при новых сообщениях
  tg t3 unsubscribe <окр> [alias]    снять подписку (тред остаётся)
  tg t3 wake <окр> <alias> [текст]   начать ход в треде шлюза и дождаться конца`

func cmdT3(ctx context.Context, args []string) int {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "", "list":
		return t3List()
	case "check":
		return t3Check(ctx, args)
	case "subscribe":
		return t3Subscribe(ctx, args)
	case "unsubscribe":
		return t3Unsubscribe(args)
	case "wake":
		return t3Wake(ctx, args)
	case "-h", "--help", "help":
		fmt.Println(t3Usage)
		return 0
	}
	fmt.Fprintln(os.Stderr, t3Usage)
	return 2
}

func t3List() int {
	envs, err := config.LoadT3()
	if err != nil {
		return fail(err)
	}
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	rows := make([]*omap.Map, 0, len(envs))
	for _, e := range envs {
		how := []string{}
		if e.TokenFile != "" {
			how = append(how, "token_file")
		}
		if len(e.PairingCmd) > 0 {
			how = append(how, "pairing_cmd")
		}
		if e.AdminTokenEnv != "" {
			state := "не задан"
			if e.AdminToken() != "" {
				state = "задан"
			}
			how = append(how, "admin_token_env ("+state+")")
		}
		rows = append(rows, omap.New().Set("name", e.Name).Set("origin", e.Origin).Set("model", e.Model).
			Set("project", e.Project).Set("token", strings.Join(how, ", ")))
	}
	printJSON(omap.New().Set("config", config.T3Path()).Set("envs", rows).Set("threads", core.RemoteSubs(s)))
	return 0
}

func t3Check(ctx context.Context, args []string) int {
	envs, err := config.LoadT3()
	if err != nil {
		return fail(err)
	}
	if len(envs) == 0 {
		fmt.Println("Окружений нет: config/t3.toml пуст или его нет (образец — config/t3.example.toml).")
		return 0
	}
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	bad := 0
	for _, e := range envs {
		if len(args) > 0 && !strings.EqualFold(args[0], e.Name) {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		out, err := core.CheckRemote(cctx, s, e.Name)
		cancel()
		if err != nil {
			bad++
			fmt.Printf("[x] %s (%s): %v\n", e.Name, e.Origin, err)
			continue
		}
		fmt.Printf("[v] %s\n", omap.Pretty(out))
	}
	if bad > 0 {
		return 1
	}
	return 0
}

func t3Subscribe(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("t3 subscribe", flag.ContinueOnError)
	after := fs.Int("after", 0, "последний обработанный id: о более новых сообщат сразу")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 2 {
		fmt.Fprintln(os.Stderr, "Использование: tg t3 subscribe <окружение> <alias> [--after N]")
		return 2
	}
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	out, err := core.SubscribeRemote(ctx, s, pos[0], pos[1], *after, "owner")
	if err != nil {
		return fail(err)
	}
	if !service.Up(ctx) {
		out.Set("service", "служба не запущена: будить тред будет некому, пока она не поднимется")
	}
	printJSON(out)
	return 0
}

func t3Unsubscribe(args []string) int {
	if len(args) < 1 || len(args) > 2 {
		fmt.Fprintln(os.Stderr, "Использование: tg t3 unsubscribe <окружение> [alias]")
		return 2
	}
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	chat := ""
	if len(args) == 2 {
		chat = args[1]
	}
	out, err := core.UnsubscribeRemote(s, args[0], chat)
	if err != nil {
		return fail(err)
	}
	printJSON(out)
	return 0
}

func t3Wake(ctx context.Context, args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Использование: tg t3 wake <окружение> <alias> [текст]")
		return 2
	}
	text := strings.TrimSpace(strings.Join(args[2:], " "))
	if text == "" {
		text = "Ответь одним словом: готов"
	}
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	out, err := core.WakeRemote(ctx, s, args[0], args[1], text)
	if err != nil {
		return fail(err)
	}
	printJSON(out)
	if st, _ := out.Get("state"); st != "completed" {
		return 1
	}
	return 0
}

// ── агенты из контейнеров ────────────────────────────────────────────────

func cmdAgents(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	newToken := fs.Bool("new-token", false, "напечатать случайный токен для .env")
	if _, err := parse(fs, args); err != nil {
		return 2
	}
	if *newToken {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return fail(err)
		}
		fmt.Println(hex.EncodeToString(b))
		return 0
	}
	cfg, err := config.LoadAgents()
	if err != nil {
		return fail(err)
	}
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	envs, _ := config.LoadT3()
	known := map[string]bool{}
	for _, e := range envs {
		known[strings.ToLower(e.Name)] = true
	}
	rows := make([]*omap.Map, 0, len(cfg.List))
	for _, a := range cfg.List {
		token := "задан"
		switch tok := a.Token(); {
		case tok == "":
			token = "НЕ ЗАДАН — агент не пустят (" + a.TokenEnv + " в .env)"
		case len(tok) < config.MinTokenLen:
			token = fmt.Sprintf("слишком короткий — нужно от %d символов (tg agents --new-token)", config.MinTokenLen)
		}
		var chats, unknown []string
		for _, c := range a.Chats {
			if _, err := s.Resolve(c); err == nil {
				chats = append(chats, c)
			} else {
				unknown = append(unknown, c)
			}
		}
		row := omap.New().Set("name", a.Name).Set("token_env", a.TokenEnv).Set("token", token).
			Set("chats", chats).Set("env", a.Env)
		if len(unknown) > 0 {
			row.Set("chats_not_in_whitelist", unknown)
		}
		if a.Env != "" && !known[strings.ToLower(a.Env)] {
			row.Set("env_problem", "окружения нет в config/t3.toml — tg_subscribe не сработает")
		}
		rows = append(rows, row)
	}
	out := omap.New().Set("config", config.AgentsPath()).Set("listen", cfg.Listen).Set("agents", rows)
	var st service.Status
	if service.Up(ctx) && service.Do(ctx, "status", nil, &st) == nil {
		state := st.MCPHTTP
		if state == "" {
			state = "выключен (нет агентов или адрес занят — см. аудит mcp_http_failed)"
		}
		out.Set("service_http", state)
	} else {
		out.Set("service_http", "служба не запущена")
	}
	printJSON(out)
	return 0
}

// doctorRemote — окружения T3 и агенты из контейнеров (без сети).
func doctorRemote(s *config.Settings, up bool, status service.Status) []string {
	var problems []string
	envs, err := config.LoadT3()
	switch {
	case err != nil:
		fmt.Println("[x]", err)
		problems = append(problems, "config/t3.toml не читается")
	case len(envs) > 0:
		names := make([]string, 0, len(envs))
		for _, e := range envs {
			names = append(names, e.Name)
		}
		fmt.Printf("[v] окружения T3 в контейнерах: %s (проверить вживую — tg t3 check)\n", strings.Join(names, ", "))
	}
	cfg, err := config.LoadAgents()
	if err != nil {
		fmt.Println("[x]", err)
		return append(problems, "config/agents.toml не читается")
	}
	if len(cfg.List) == 0 {
		return problems
	}
	for _, a := range cfg.List {
		if len(a.Token()) < config.MinTokenLen {
			problems = append(problems, fmt.Sprintf("у агента %s нет токена в .env (%s) или он короче %d символов", a.Name, a.TokenEnv, config.MinTokenLen))
		}
		for _, c := range a.Chats {
			if _, err := s.Resolve(c); err != nil {
				problems = append(problems, fmt.Sprintf("у агента %s чат %q не из белого списка — он ему не виден", a.Name, c))
			}
		}
	}
	switch {
	case !up:
		fmt.Printf("[?] MCP по HTTP (%s): служба не запущена\n", cfg.Listen)
	case status.MCPHTTP != "":
		fmt.Printf("[v] MCP по HTTP: http://%s/mcp, агентов: %d\n", status.MCPHTTP, len(cfg.List))
	default:
		fmt.Printf("[x] MCP по HTTP (%s) не слушает\n", cfg.Listen)
		problems = append(problems, "служба не подняла MCP по HTTP: адрес занят или старая сборка — см. mcp_http_failed в аудите")
	}
	return problems
}
