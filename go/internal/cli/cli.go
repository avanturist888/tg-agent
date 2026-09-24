// Package cli — командная строка для владельца аккаунта. Агенты сюда не
// ходят — у них MCP.
package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"tgagent/internal/audit"
	"tgagent/internal/bot"
	"tgagent/internal/chatsfile"
	"tgagent/internal/config"
	"tgagent/internal/core"
	"tgagent/internal/lock"
	"tgagent/internal/mcpserver"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/tgc"
)

// GUI — запуск окна управления (подставляется из cmd, чтобы cli не тянул WebView).
var GUI func(ctx context.Context, args []string) error

type command struct {
	name  string
	help  string
	run   func(ctx context.Context, args []string) int
	flags func(fs *flag.FlagSet)
}

func printJSON(v any) { fmt.Println(omap.Pretty(v)) }

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "Ошибка:", err)
	return 1
}

// parse — флаги в любом месте строки, как у argparse: `allow x --send`.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func settings() (*config.Settings, error) { return config.Load() }

var commands []command

func init() {
	commands = []command{
		{name: "login", help: "вход и сохранение сессии", run: cmdLogin},
		{name: "logout", help: "отозвать сессию на стороне Telegram", run: cmdLogout},
		{name: "whoami", help: "под каким аккаунтом работаем", run: cmdWhoami},
		{name: "dialogs", help: "все диалоги (вкл. архив) — искать чат по имени", run: cmdDialogs},
		{name: "allow", help: "добавить чат в белый список или поменять права", run: cmdAllow},
		{name: "deny", help: "убрать чат из белого списка", run: cmdDeny},
		{name: "chats", help: "что разрешено агентам (+ непрочитанные)", run: cmdChats},
		{name: "read", help: "прочитать чат", run: cmdRead},
		{name: "drafts", help: "очередь черновиков от агентов", run: cmdDrafts},
		{name: "pending", help: "что ждёт нажатия кнопки", run: cmdPending},
		{name: "approvals", help: "слушатель кнопок: демон или разбор накопившегося (--once)", run: cmdApprovals},
		{name: "approve", help: "подтвердить отправку (--send — и отправить)", run: cmdApprove},
		{name: "reject", help: "отменить черновик", run: cmdReject},
		{name: "send", help: "отправить подтверждённый черновик вручную", run: cmdSend},
		{name: "watch", help: "подписка: «alias id» на каждое новое сообщение (для Monitor)", run: cmdWatch},
		{name: "feeds", help: "состояние лент новых сообщений", run: cmdFeeds},
		{name: "doctor", help: "проверить конфигурацию", run: cmdDoctor},
		{name: "mcp", help: "MCP-сервер для Claude Code (stdio)", run: cmdMCP},
		{name: "gui", help: "окно управления: чаты, черновики, настройки", run: cmdGUI},
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "tg-agent (Go) — доступ агентов Claude Code к Telegram по белому списку чатов.\n\nКоманды:")
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  %-10s %s\n", c.name, c.help)
	}
}

// Main — точка входа; возвращает код выхода.
func Main(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage()
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	for _, c := range commands {
		if c.name == args[0] {
			return c.run(ctx, args[1:])
		}
	}
	fmt.Fprintf(os.Stderr, "Неизвестная команда: %s\n\n", args[0])
	usage()
	return 2
}

// ── вход ─────────────────────────────────────────────────────────────────

type terminal struct {
	phone string
	in    *bufio.Reader
}

func (t *terminal) ask(prompt string) (string, error) {
	fmt.Print(prompt)
	line, err := t.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (t *terminal) Phone(context.Context) (string, error) {
	if t.phone != "" {
		return t.phone, nil
	}
	return t.ask("Телефон (+7...): ")
}

func (t *terminal) Code(context.Context) (string, error) {
	return t.ask("Код из Telegram: ")
}

func (t *terminal) Password(context.Context) (string, error) {
	fmt.Print("Пароль двухфакторной защиты: ")
	if term.IsTerminal(int(os.Stdin.Fd())) {
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		return string(raw), err
	}
	line, err := t.in.ReadString('\n')
	return strings.TrimSpace(line), err
}

func cmdLogin(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	phone := fs.String("phone", "", "номер телефона")
	if _, err := parse(fs, args); err != nil {
		return 2
	}
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	p := *phone
	if p == "" {
		p = s.Phone
	}
	me, err := tgc.Login(ctx, s, &terminal{phone: p, in: bufio.NewReader(os.Stdin)})
	if err != nil {
		return fail(err)
	}
	_ = audit.Log(s.AuditPath(), "login", "user_id", me.ID, "username", me.Username, "impl", "go")
	fmt.Printf("Готово. Сессия сохранена: %s\n", s.SessionPath)
	username := me.Username
	if username == "" {
		username = "—"
	}
	fmt.Printf("Аккаунт: %s @%s (id %d)\n", me.FirstName, username, me.ID)
	return 0
}

func cmdLogout(ctx context.Context, args []string) int {
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	if err := tgc.Logout(ctx, s); err != nil {
		return fail(err)
	}
	_ = audit.Log(s.AuditPath(), "logout", "impl", "go")
	fmt.Println("Сессия отозвана на стороне Telegram.")
	return 0
}

func cmdWhoami(ctx context.Context, args []string) int {
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	err = tgc.Run(ctx, s, tgc.Opts{}, func(ctx context.Context, c *tgc.Conn) error {
		me, err := c.Self(ctx)
		if err != nil {
			return err
		}
		c.Peers.Save()
		var username any
		if me.Username != "" {
			username = me.Username
		}
		printJSON(omap.New().Set("id", me.ID).Set("username", username).Set("name", me.FirstName).
			Set("session", s.SessionPath).Set("send_policy", s.SendPolicy).Set("chats_allowed", len(s.Chats)))
		return nil
	})
	if err != nil {
		return fail(err)
	}
	return 0
}

// ── белый список ─────────────────────────────────────────────────────────

func cmdDialogs(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("dialogs", flag.ContinueOnError)
	filter := fs.String("filter", "", "подстрока в названии")
	limit := fs.Int("limit", 200, "сколько диалогов смотреть")
	if _, err := parse(fs, args); err != nil {
		return 2
	}
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	var rows []tgc.DialogRow
	err = tgc.Run(ctx, s, tgc.Opts{}, func(ctx context.Context, c *tgc.Conn) error {
		rows, err = c.ListDialogs(ctx, *limit)
		return err
	})
	if err != nil {
		return fail(err)
	}
	needle := strings.ToLower(*filter)
	shown := 0
	for _, r := range rows {
		if needle != "" && !strings.Contains(strings.ToLower(r.Title), needle) {
			continue
		}
		uname, flagArch := "", ""
		if r.Username != "" {
			uname = " @" + r.Username
		}
		if r.Archived {
			flagArch = " [архив]"
		}
		fmt.Printf("%16d  %-7s %s%s%s\n", r.ID, r.Kind, r.Title, uname, flagArch)
		shown++
	}
	if shown == 0 {
		fmt.Fprintln(os.Stderr, "Ничего не нашлось. Попробуй другой --filter или увеличь --limit.")
		return 1
	}
	fmt.Fprintf(os.Stderr, "\nНайдено: %d. Добавить в белый список: tg allow <alias> --id <id>\n", shown)
	return 0
}

func cmdAllow(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("allow", flag.ContinueOnError)
	id := fs.String("id", "", "id чата из tg dialogs, @username или me")
	match := fs.String("match", "", "найти чат по куску названия")
	title := fs.String("title", "", "название для людей")
	send := fs.Bool("send", false, "разрешить отправку (с подтверждением)")
	auto := fs.Bool("auto", false, "автоотправка без кнопки, с окном на отмену (вместе с --send)")
	noRead := fs.Bool("no-read", false, "запретить чтение")
	note := fs.String("note", "", "заметка для агента")
	limit := fs.Int("limit", 200, "сколько диалогов смотреть для --match")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "Использование: tg allow <alias> [--id X | --match X] [--send [--auto]] [--no-read]")
		return 2
	}
	alias := pos[0]
	if *auto && !*send {
		fmt.Fprintln(os.Stderr, "--auto работает только вместе с --send.")
		return 2
	}
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	if _, exists := findAlias(s, alias); exists {
		if *id != "" || *match != "" {
			fmt.Fprintf(os.Stderr, "Алиас '%s' уже занят другим чатом.\n", alias)
			return 1
		}
		var notePtr *string
		if *note != "" {
			notePtr = note
		}
		if err := chatsfile.Update(alias, !*noRead, *send, *auto, nil, notePtr); err != nil {
			return fail(err)
		}
		fmt.Printf("Права '%s' обновлены: read = %t, send = %t, auto_send = %t\n", alias, !*noRead, *send, *send && *auto)
		return 0
	}

	var peer any
	name := *title
	if *id != "" {
		if n, err := strconv.ParseInt(*id, 10, 64); err == nil {
			peer = n
		} else {
			peer = *id
		}
	}
	if *match != "" {
		var rows []tgc.DialogRow
		err := tgc.Run(ctx, s, tgc.Opts{}, func(ctx context.Context, c *tgc.Conn) error {
			var err error
			rows, err = c.ListDialogs(ctx, *limit)
			return err
		})
		if err != nil {
			return fail(err)
		}
		needle := strings.ToLower(*match)
		var found []tgc.DialogRow
		for _, r := range rows {
			if strings.Contains(strings.ToLower(r.Title), needle) {
				found = append(found, r)
			}
		}
		switch {
		case len(found) == 0:
			fmt.Fprintf(os.Stderr, "По '%s' ничего не нашлось.\n", *match)
			return 1
		case len(found) > 1:
			fmt.Fprintln(os.Stderr, "Под запрос подходит несколько чатов — уточни --match или задай --id:")
			for _, r := range found[:min(10, len(found))] {
				fmt.Printf("  %16d  %-7s %s\n", r.ID, r.Kind, r.Title)
			}
			return 1
		}
		peer = found[0].ID
		if name == "" {
			name = found[0].Title
		}
	}
	if peer == nil {
		fmt.Fprintln(os.Stderr, "Нужен --id или --match.")
		return 1
	}
	c := chatsfile.Chat{Alias: alias, Peer: peer, Title: strings.ReplaceAll(name, `"`, "'"), Read: !*noRead, Send: *send, Auto: *auto, Note: *note}
	if err := chatsfile.Add(c); err != nil {
		return fail(err)
	}
	fmt.Printf("Добавлено в белый список:%s", chatsfile.Block(c))
	fmt.Println("Перезапускать ничего не нужно — сервер перечитает файл на следующем вызове.")
	return 0
}

func findAlias(s *config.Settings, alias string) (config.ChatRule, bool) {
	for a, r := range s.Chats {
		if strings.EqualFold(a, alias) {
			return r, true
		}
	}
	return config.ChatRule{}, false
}

func cmdDeny(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("deny", flag.ContinueOnError)
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "Использование: tg deny <alias>")
		return 2
	}
	if err := chatsfile.Remove(pos[0]); err != nil {
		return fail(err)
	}
	fmt.Printf("Чат '%s' убран из белого списка.\n", pos[0])
	return 0
}

func cmdChats(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("chats", flag.ContinueOnError)
	status := fs.Bool("status", false, "непрочитанные и последнее сообщение")
	if _, err := parse(fs, args); err != nil {
		return 2
	}
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	out, err := core.ListChats(ctx, s, *status)
	if err != nil {
		return fail(err)
	}
	printJSON(out)
	return 0
}

func cmdRead(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	limit := fs.Int("limit", 20, "сколько сообщений")
	search := fs.String("search", "", "поиск по тексту")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "Использование: tg read <alias> [--limit N] [--search x]")
		return 2
	}
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	out, err := core.ReadChat(ctx, s, pos[0], *limit, 0, 0, *search, true)
	if err != nil {
		return fail(err)
	}
	printJSON(out)
	return 0
}

// ── черновики ────────────────────────────────────────────────────────────

func cmdDrafts(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("drafts", flag.ContinueOnError)
	status := fs.String("status", "", "pending / approved / scheduled / sent / cancelled / expired")
	asJSON := fs.Bool("json", false, "вывести JSON")
	if _, err := parse(fs, args); err != nil {
		return 2
	}
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	drafts, err := outbox.New(s.OutboxPath()).List(*status)
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		out, _ := core.ListDrafts(s, *status)
		printJSON(out)
		return 0
	}
	if len(drafts) == 0 {
		fmt.Println("Черновиков нет.")
		return 0
	}
	for _, d := range drafts {
		fmt.Printf("[%s] %s → %s  (истекает %s)\n", d.Status, d.ID, d.Chat, d.ExpiresAt)
		for _, line := range strings.Split(d.Text, "\n") {
			fmt.Println("    " + line)
		}
		fmt.Println()
	}
	return 0
}

func cmdPending(ctx context.Context, args []string) int {
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	rows, err := core.PendingSummary(s)
	if err != nil {
		return fail(err)
	}
	if len(rows) == 0 {
		fmt.Println("Ничего не ждёт подтверждения.")
		return 0
	}
	for _, d := range rows {
		card := "карточки нет"
		if d.BotMessageID != nil {
			card = "карточка в боте"
		}
		fmt.Printf("%s → %s  (%s, создан %s)\n", d.ID, d.Chat, card, d.CreatedAt)
		for _, line := range strings.Split(d.Text, "\n") {
			fmt.Println("    " + line)
		}
		fmt.Println()
	}
	return 0
}

func cmdApprove(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	send := fs.Bool("send", false, "сразу и отправить")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "Использование: tg approve <draft_id> [--send]")
		return 2
	}
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	box := outbox.New(s.OutboxPath())
	d, err := box.Get(pos[0])
	if err != nil {
		return fail(err)
	}
	fmt.Printf("Чат: %s\n--- текст ---\n%s\n-------------\n", d.Chat, d.Text)
	if d.Status != outbox.Pending {
		fmt.Printf("Статус: %s — подтверждать нечего.\n", d.Status)
		return 1
	}
	if _, err := box.Approve(pos[0], "human"); err != nil {
		return fail(err)
	}
	_ = audit.Log(s.AuditPath(), "approve", "draft_id", pos[0], "chat", d.Chat, "by", "human")
	fmt.Println("Подтверждено:", pos[0])
	if *send {
		out, err := core.SendDraft(ctx, s, pos[0], "tg approve --send")
		if err != nil {
			return fail(err)
		}
		printJSON(out)
	} else {
		fmt.Println("Агент может отправить его через tg_send_draft (или запусти `tg send <id>`).")
	}
	return 0
}

func cmdReject(ctx context.Context, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Использование: tg reject <draft_id>")
		return 2
	}
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	out, err := core.CancelDraft(ctx, s, args[0], "human")
	if err != nil {
		return fail(err)
	}
	printJSON(out)
	return 0
}

func cmdSend(ctx context.Context, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Использование: tg send <draft_id>")
		return 2
	}
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	out, err := core.SendDraft(ctx, s, args[0], "cli send by owner")
	if err != nil {
		return fail(err)
	}
	printJSON(out)
	return 0
}

// ── слушатель и ленты ────────────────────────────────────────────────────

func cmdApprovals(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("approvals", flag.ContinueOnError)
	once := fs.Bool("once", false, "разобрать накопившиеся нажатия и выйти")
	if _, err := parse(fs, args); err != nil {
		return 2
	}
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	if !s.BotReady() {
		fmt.Fprintln(os.Stderr, "Бот подтверждений не настроен: нет токена или chat_id.")
		return 1
	}
	if *once {
		n, err := core.SweepOnce(ctx, s)
		if err != nil {
			return fail(err)
		}
		fmt.Println("Разобрано накопившихся нажатий:", n)
		return 0
	}
	if err := core.RunDaemon(ctx, s, config.Load); err != nil && !errors.Is(err, context.Canceled) {
		return fail(err)
	}
	return 0
}

func cmdWatch(ctx context.Context, args []string) int {
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	if err := core.Watch(ctx, s, args, os.Stdout); err != nil {
		return fail(err)
	}
	return 0
}

func cmdFeeds(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("feeds", flag.ContinueOnError)
	poll := fs.Bool("poll", false, "сделать один проход сейчас")
	if _, err := parse(fs, args); err != nil {
		return 2
	}
	s, err := settings()
	if err != nil {
		return fail(err)
	}
	if *poll {
		added, err := core.PollOnce(ctx, s, false)
		if err != nil {
			return fail(err)
		}
		if len(added) == 0 {
			fmt.Println("добавлено: ничего нового")
		} else {
			fmt.Println("добавлено:", added)
		}
	}
	for _, r := range s.Rules() {
		if r.Read {
			path := core.FeedPath(s, r.Alias)
			fmt.Printf("  %-28s последний id %-10d %s\n", r.Alias, core.LastID(path), path)
		}
	}
	return 0
}

func cmdMCP(ctx context.Context, args []string) int {
	if err := mcpserver.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "mcp:", err)
		return 1
	}
	return 0
}

func cmdGUI(ctx context.Context, args []string) int {
	if GUI == nil {
		fmt.Fprintln(os.Stderr, "Эта сборка без окна управления.")
		return 1
	}
	if err := GUI(ctx, args); err != nil {
		return fail(err)
	}
	return 0
}

// ── doctor ───────────────────────────────────────────────────────────────

const exampleAPIID = 1234567

func cmdDoctor(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	offline := fs.Bool("offline", false, "не обращаться к Telegram")
	if _, err := parse(fs, args); err != nil {
		return 2
	}
	var problems []string
	s, err := settings()
	if err != nil {
		fmt.Println("[x]", err)
		return 1
	}
	fmt.Printf("[v] .env прочитан, api_id=%d, корень %s\n", s.APIID, config.Root)
	if s.APIID == exampleAPIID {
		problems = append(problems, "в .env остались значения-заглушки из .env.example — впиши свои с my.telegram.org")
	}
	switch {
	case *offline:
		fmt.Printf("[?] сессия: %s (проверка пропущена, --offline)\n", s.SessionPath)
	case s.APIID == exampleAPIID:
		fmt.Println("[?] сессия: не проверяю, пока не заданы настоящие ключи")
	default:
		tctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		err := tgc.Run(tctx, s, tgc.Opts{}, func(ctx context.Context, c *tgc.Conn) error {
			me, err := c.Self(ctx)
			if err != nil {
				return err
			}
			c.Peers.Save()
			u := me.Username
			if u == "" {
				u = "—"
			}
			fmt.Printf("[v] авторизован: %s @%s (id %d)\n", me.FirstName, u, me.ID)
			return nil
		})
		cancel()
		var nl *tgc.NotLoggedIn
		switch {
		case err == nil:
		case errors.As(err, &nl):
			fmt.Println("[x] сессия:", s.SessionPath)
			problems = append(problems, "аккаунт не подключён — выполни `tg login`")
		default:
			fmt.Printf("[?] сессия: не удалось проверить (%v)\n", err)
		}
	}
	fmt.Println("[v] политика отправки:", s.SendPolicy)
	if s.SendPolicy == "bot_approval" {
		if *offline {
			fmt.Printf("[?] бот подтверждений: chat %d (не проверяю, --offline)\n", s.ApprovalChatID)
		} else {
			name, err := bot.Me(ctx, s)
			if err != nil {
				fmt.Println("[x] бот подтверждений:", err)
				problems = append(problems, "бот недоступен — кнопки подтверждения работать не будут")
			} else {
				fmt.Printf("[v] бот подтверждений: @%s → chat %d\n", name, s.ApprovalChatID)
			}
		}
		if lock.Held(s.UpdatesLockPath()) {
			fmt.Println("[v] слушатель кнопок работает (`tg approvals`)")
		} else {
			fmt.Println("[?] слушатель кнопок не запущен — нажатие сработает, только пока агент ждёт")
		}
	}
	fmt.Printf("[v] чатов в белом списке: %d (автоотправка через %d с)\n", len(s.Chats), s.AutoSendDelaySec)
	for _, r := range s.Rules() {
		flags := []byte("---")
		if r.Read {
			flags[0] = 'r'
		}
		if r.Send {
			flags[1] = 'w'
		}
		if r.AutoSend() {
			flags[2] = 'a'
		}
		fmt.Printf("    [%s] %-16s %s  %s\n", flags, r.Alias, r.PeerString(), r.Title)
	}
	if len(problems) > 0 {
		fmt.Println("\nЧто поправить:")
		for _, p := range problems {
			fmt.Println("  -", p)
		}
		return 1
	}
	fmt.Println("\nВсё в порядке.")
	return 0
}
