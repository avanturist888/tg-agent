package core

// Действия с ботами через BotFather (tg_botfather) и хранилище их токенов.
//
// Токен бота — полный доступ к нему, а ответ BotFather несёт его открытым
// текстом. Поэтому диалог ведёт служба, а не агент: действие — обычный
// черновик в чат BotFather (то же подтверждение или автоотправка, что у
// сообщений), токен из ответа служба проверяет (getMe), кладёт в хранилище
// (диспетчер учётных данных) и сразу удаляет сообщение с ним из чата. Агент
// видит только итог: какой бот, сохранён ли токен. Сам токен — трубой
// `tg bot-token @bot | …` или записью в .env по просьбе агента (env_file).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tgagent/internal/bot"
	"tgagent/internal/config"
	"tgagent/internal/fsx"
	"tgagent/internal/lock"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/secret"
	"tgagent/internal/store"
	"tgagent/internal/tgc"
	"tgagent/internal/wincred"
)

const botFatherID int64 = 93372553

// botActions — что умеет tg_botfather: шаги диалога и что для них нужно.
var botActions = map[string]struct {
	cmd      string
	needBot  bool
	value    string // "" — значение не нужно; иначе — что в нём
	token    bool   // в ответе будет токен
	confirm  string // последний шаг — подтверждение
	describe string
}{
	"newbot":         {cmd: "/newbot", token: true, describe: "завести бота: name, username"},
	"token":          {cmd: "/token", needBot: true, token: true, describe: "взять действующий токен в хранилище"},
	"revoke":         {cmd: "/revoke", needBot: true, token: true, describe: "перевыпустить токен"},
	"setname":        {cmd: "/setname", needBot: true, value: "имя", describe: "сменить имя"},
	"setdescription": {cmd: "/setdescription", needBot: true, value: "описание", describe: "описание (до 512)"},
	"setabouttext":   {cmd: "/setabouttext", needBot: true, value: "текст «О боте»", describe: "«О боте» (до 120)"},
	"setcommands":    {cmd: "/setcommands", needBot: true, value: "строки «команда - описание»", describe: "список команд"},
	"deletecommands": {cmd: "/deletecommands", needBot: true, describe: "убрать список команд"},
	"setprivacy":     {cmd: "/setprivacy", needBot: true, value: "Enable или Disable", describe: "режим приватности в группах"},
	"setjoingroups":  {cmd: "/setjoingroups", needBot: true, value: "Enable или Disable", describe: "можно ли звать в группы"},
	"setinline":      {cmd: "/setinline", needBot: true, value: "подсказка поля ввода", describe: "инлайн-режим"},
	"deletebot":      {cmd: "/deletebot", needBot: true, confirm: "Yes, I am totally sure.", describe: "удалить бота навсегда"},
	"purge_tokens":   {describe: "убрать из чата все сообщения с токенами"},
}

var botUsername = regexp.MustCompile(`^@?[A-Za-z][A-Za-z0-9_]{3,30}[Bb][Oo][Tt]$`)

// botFatherRule — чат BotFather в белом списке (нужна отправка).
func botFatherRule(s *config.Settings) (config.ChatRule, error) {
	for _, r := range s.Rules() {
		peer := strings.ToLower(strings.TrimSpace(r.PeerString()))
		if rulePeerID(r) == botFatherID || peer == "@botfather" || peer == "botfather" {
			if !r.Send {
				return r, denied("Чат BotFather открыт только на чтение — действия с ботами требуют отправки.")
			}
			return r, nil
		}
	}
	return config.ChatRule{}, denied("Чата BotFather нет в белом списке. Попроси владельца: " +
		"tg_request_access(chat_id=93372553, send=true, reason=…).")
}

// DraftBotOp — tg_botfather: действие в BotFather через черновик.
func DraftBotOp(ctx context.Context, s *config.Settings, b outbox.BotSpec, note string) (*omap.Map, error) {
	if n := netOf(s); n != nil {
		return nil, denied("BotFather — это Telegram; в %s ботов так не заводят.", n.name())
	}
	b.Action = strings.ToLower(strings.TrimSpace(b.Action))
	spec, ok := botActions[b.Action]
	if !ok {
		var names []string
		for k, v := range botActions {
			names = append(names, k+" ("+v.describe+")")
		}
		sort.Strings(names)
		return nil, &Bad{Msg: "Действие — одно из: " + strings.Join(names, ", ") + "."}
	}
	if spec.needBot {
		if !botUsername.MatchString(strings.TrimSpace(b.Bot)) {
			return nil, &Bad{Msg: "bot — @username бота (кончается на bot)."}
		}
		b.Bot = "@" + strings.TrimPrefix(strings.TrimSpace(b.Bot), "@")
	} else {
		b.Bot = ""
	}
	if b.Action == "newbot" {
		b.Name = strings.TrimSpace(b.Name)
		b.Username = strings.TrimPrefix(strings.TrimSpace(b.Username), "@")
		if b.Name == "" || !botUsername.MatchString(b.Username) {
			return nil, &Bad{Msg: "newbot: name — отображаемое имя, username — латиница, 5–32 символа, кончается на bot."}
		}
	}
	b.Value = strings.TrimSpace(b.Value)
	if spec.value != "" && b.Value == "" {
		return nil, &Bad{Msg: fmt.Sprintf("%s: нужно value — %s.", b.Action, spec.value)}
	}
	if spec.value == "" {
		b.Value = ""
	}
	if b.EnvFile != "" || b.EnvKey != "" {
		if !spec.token {
			return nil, &Bad{Msg: "env_file — только для действий с токеном: newbot, token, revoke."}
		}
		if err := checkEnvTarget(s, b.EnvFile, b.EnvKey); err != nil {
			return nil, err
		}
	}
	rule, err := botFatherRule(s)
	if err != nil {
		return nil, err
	}
	opts := outbox.CreateOpts{Chat: rule.Alias, TTLMin: s.DraftTTLMin, Note: note,
		Origin: draftOrigin(s), Bot: &b}
	// удаление бота необратимо: только кнопкой владельца, даже в чате с автоотправкой
	auto := rule.AutoSend() && b.Action != "deletebot"
	if auto {
		at := time.Now().Add(time.Duration(s.AutoSendDelaySec) * time.Second)
		opts.SendAt = &at
	}
	d, err := outbox.New(s.DataDir()).Create(opts)
	if err != nil {
		return nil, err
	}
	logEvent(s, "draft_botfather", "draft_id", d.ID, "op", b.Action, "bot", cmpOr(b.Bot, b.Username),
		"env_file", b.EnvFile)
	out := omap.New().Set("draft_id", d.ID).Set("chat", s.Ref(rule.Alias)).Set("action", b.Action).
		Set("what", bot.BotLine(&b)).Set("expires_at", d.ExpiresAt)
	return finishDraft(ctx, s, rule, d, out, note, auto)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// checkEnvTarget — куда агент просит записать токен: только .env-файл с
// абсолютным путём на этой машине и обычное имя переменной.
func checkEnvTarget(s *config.Settings, file, key string) error {
	if s.Agent != nil {
		return denied("Агенту из контейнера писать файлы на диске шлюза нельзя: токен возьмёт владелец (tg bot-token).")
	}
	if !filepath.IsAbs(file) || !envKey.MatchString(key) {
		return &Bad{Msg: "env_file — абсолютный путь к файлу .env (или *.env), env_key — имя переменной (TG_BOT_TOKEN)."}
	}
	base := strings.ToLower(filepath.Base(file))
	if base != ".env" && !strings.HasSuffix(base, ".env") {
		return &Bad{Msg: "env_file — только файл .env или *.env: токен пишется строкой KEY=…"}
	}
	return nil
}

// ── выполнение (sendDraft) ───────────────────────────────────────────────

type botConn interface {
	BotDialog(ctx context.Context, t tgc.Target, steps []string, stop func(string) bool) (int, []tgc.BotReply, error)
	DeleteMessages(ctx context.Context, t tgc.Target, ids []int) error
	History(ctx context.Context, t tgc.Target, limit, beforeID, afterID int, search string, topic int) (*tgc.Batch, error)
}

// botRefused — BotFather не сделал, что просили.
func botRefused(text string) bool {
	low := strings.ToLower(text)
	for _, w := range []string{"sorry", "invalid", "unrecognized", "can't", "cannot", "error", "not allowed",
		"is already taken", "too long", "must end"} {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

// runBotOp — провести действие в BotFather; итог агенту (без токена).
func runBotOp(ctx context.Context, s *config.Settings, c botConn, t tgc.Target, d *outbox.Draft) (*omap.Map, error) {
	b := d.Bot
	spec := botActions[b.Action]
	if b.Action == "purge_tokens" {
		return purgeTokens(ctx, s, c, t, d.ID)
	}
	steps := []string{spec.cmd}
	switch {
	case b.Action == "newbot":
		steps = append(steps, b.Name, b.Username)
	case spec.needBot:
		steps = append(steps, b.Bot)
	}
	if b.Value != "" {
		steps = append(steps, b.Value)
	}
	if spec.confirm != "" {
		steps = append(steps, spec.confirm)
	}
	first, replies, err := c.BotDialog(ctx, t, steps, botRefused)
	out := omap.New().Set("message_id", first).Set("action", b.Action)
	masked := make([]string, 0, len(replies))
	for _, r := range replies {
		masked = append(masked, secret.Mask(r.Text))
	}
	out.Set("botfather_replies", masked)
	if err != nil {
		saveBotOp(s, d.ID, out.Set("error", err.Error()))
		return nil, err
	}
	if len(replies) > 0 && botRefused(replies[len(replies)-1].Text) {
		_, _, _ = c.BotDialog(ctx, t, []string{"/cancel"}, func(string) bool { return true })
		saveBotOp(s, d.ID, out.Set("refused", true))
		// отказ BotFather повтором не лечится — Denied, без повторных попыток
		return nil, denied("BotFather отказал: %s", secret.Mask(replies[len(replies)-1].Text))
	}
	if spec.token {
		saved, err := keepTokens(ctx, s, c, t, replies, d.ID, b)
		if err != nil {
			saveBotOp(s, d.ID, out.Set("error", err.Error()))
			return nil, err
		}
		out.Set("token_saved", saved)
		if b.EnvFile != "" {
			out.Set("env_written", b.EnvFile+" ("+b.EnvKey+")")
		}
		out.Set("token_hint", "Токен в хранилище tg-agent, сообщение с ним удалено из чата. Взять — только трубой: "+
			"bin\\tg.exe bot-token "+saved+" | … ; проверять по getMe, не печатая.")
	}
	logEvent(s, "botfather", "draft_id", d.ID, "op", b.Action, "bot", cmpOr(b.Bot, "@"+b.Username),
		"replies", len(replies))
	saveBotOp(s, d.ID, out)
	return out, nil
}

// keepTokens — токены из ответов: проверить, сохранить, удалить из чата.
func keepTokens(ctx context.Context, s *config.Settings, c botConn, t tgc.Target, replies []tgc.BotReply,
	draftID string, b *outbox.BotSpec) (string, error) {
	var (
		ids  []int
		name string
	)
	for _, r := range replies {
		toks := secret.BotTokens(r.Text)
		if len(toks) == 0 {
			continue
		}
		ids = append(ids, r.ID)
		for _, tok := range toks {
			user, err := saveBotToken(ctx, s, tok, "tg_botfather "+b.Action+" ("+draftID+")")
			if err != nil {
				return "", err
			}
			name = user
			if b.EnvFile != "" {
				if err := writeEnv(b.EnvFile, b.EnvKey, tok); err != nil {
					return name, fmt.Errorf("токен сохранён в хранилище, но в %s не записан: %w", b.EnvFile, err)
				}
				noteEnv(user, b.EnvFile+" ("+b.EnvKey+")")
			}
		}
	}
	if name == "" {
		return "", errors.New("в ответе BotFather нет токена — см. botfather_replies")
	}
	if err := c.DeleteMessages(ctx, t, ids); err != nil {
		logEvent(s, "bot_token_delete_failed", "draft_id", draftID, "error", err.Error())
		return name, fmt.Errorf("токен сохранён, но сообщение с ним не удалилось из чата: %w", err)
	}
	return name, nil
}

// purgeTokens — убрать из чата все сообщения BotFather с токенами;
// действующие токены (getMe отвечает) сначала — в хранилище, если их там нет.
func purgeTokens(ctx context.Context, s *config.Settings, c botConn, t tgc.Target, draftID string) (*omap.Map, error) {
	b, err := c.History(ctx, t, 3000, 0, 0, "", 0)
	if err != nil {
		return nil, err
	}
	var ids []int
	kept := map[string]bool{}
	dead := 0
	// от новых к старым: у бота сохраняется самый свежий токен
	for _, m := range b.Messages {
		id, text, out := messageText(m)
		if out {
			continue
		}
		toks := secret.BotTokens(text)
		if len(toks) == 0 {
			continue
		}
		ids = append(ids, id)
		for _, tok := range toks {
			botID, _, _ := strings.Cut(tok, ":")
			if kept[botID] {
				continue
			}
			if vaultHas(botID, tok) {
				kept[botID] = true
				continue
			}
			if _, err := saveBotToken(ctx, s, tok, "чат BotFather ("+draftID+")"); err != nil {
				var bad *botTokenDead
				if errors.As(err, &bad) {
					dead++
					continue
				}
				return nil, err // не проверили — не удаляем: это может быть единственная копия
			}
			kept[botID] = true
		}
	}
	if err := c.DeleteMessages(ctx, t, ids); err != nil {
		return nil, err
	}
	out := omap.New().Set("action", "purge_tokens").Set("deleted_messages", len(ids)).
		Set("tokens_kept", len(kept)).Set("dead_tokens", dead)
	logEvent(s, "botfather_purge", "draft_id", draftID, "deleted", len(ids), "kept", len(kept), "dead", dead)
	saveBotOp(s, draftID, out)
	return out, nil
}

func messageText(m any) (int, string, bool) {
	if msg, ok := m.(interface {
		GetID() int
		GetMessage() string
		GetOut() bool
	}); ok {
		return msg.GetID(), msg.GetMessage(), msg.GetOut()
	}
	return 0, "", false
}

// ── хранилище токенов ────────────────────────────────────────────────────

// BotEntry — бот в хранилище: сведения — таблица bots в state.db, сам токен —
// в диспетчере учётных данных Windows (botCredTarget), не файлом: так он не
// уходит в бэкап и копию папки и не находится поиском по data/.
type BotEntry struct {
	Username string   `json:"username"`
	BotID    string   `json:"bot_id"`
	Name     string   `json:"name,omitempty"`
	Token    string   `json:"token,omitempty"` // только у старых записей: при чтении переезжает в диспетчер
	SavedAt  string   `json:"saved_at"`
	Source   string   `json:"source"`
	Env      []string `json:"env,omitempty"` // куда ещё записан
}

// vaultMu — в этом процессе по очереди: запись токена в диспетчер и
// сведений о боте в базу (таблица bots основного data/). Между процессами
// сведения меняются по одной записи в транзакции (editBot), а не заменой
// всей таблицы снимком: иначе снимок стёр бы записи, добавленные другим
// процессом (или переносом bot_tokens.json) после его чтения.
var vaultMu sync.Mutex

var botsTab = store.NewTable[*BotEntry](store.TBots)

func vaultDB() (*store.DB, error) { return store.Open(config.DataPath()) }

// bot_tokens.json до переезда: токены, ещё лежащие в нём, — сначала в
// диспетчер учётных данных (в базе секретов нет); не записался хоть один —
// перенос откладывается, файл остаётся как был. Токен в самом файле — только
// у записей времён до диспетчера (сборки с ним клали туда и новые токены):
// если в диспетчере у бота уже есть токен, он новее — остаётся он.
func init() {
	store.RegisterLegacy(store.Legacy{File: "bot_tokens.json", Import: importVault})
}

func importVault(ctx context.Context, tx *sql.Tx, path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return 0, nil
	}
	m := map[string]*BotEntry{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, fmt.Errorf("%w: %v", store.ErrCorrupt, err)
	}
	moved := false
	err = lock.With(ctx, vaultLockPath(), vaultLockWait, func() error {
		for key, e := range m {
			if e == nil || e.Token == "" {
				continue
			}
			if _, ok := vaultToken(key); !ok {
				if err := credSet(botCredTarget(key), e.Token); err != nil {
					return fmt.Errorf("%w: токен @%s не записался в диспетчер: %v", store.ErrRetry, key, err)
				}
			}
			e.Token, moved = "", true
		}
		return nil
	})
	if lock.IsBusy(err) {
		err = fmt.Errorf("%w: %v", store.ErrRetry, err)
	}
	if err != nil {
		return 0, err
	}
	for key, e := range m {
		if e == nil {
			continue
		}
		if _, err := botsTab.Insert(ctx, tx, key, e); err != nil {
			return 0, err
		}
	}
	if moved {
		// файл отложат как .migrated — токенов в нём оставаться не должно:
		// все уже в диспетчере
		clean, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return 0, err
		}
		if err := fsx.WriteFile(path, clean, 0o600); err != nil {
			return 0, fmt.Errorf("%w: не вычистить токены из %s: %v", store.ErrRetry, filepath.Base(path), err)
		}
	}
	return len(m), nil
}

// vaultLockPath — лок «прочитать токен в диспетчере → записать» между
// процессами (перенос и сохранение токена). Записи диспетчера общие для
// всех профилей и установок этого пользователя Windows, поэтому и лок — в
// его каталоге, а не в data/ профиля. Под ним — только диспетчер, без
// транзакций базы: перенос берёт его внутри своей транзакции.
func vaultLockPath() string {
	dir, err := os.UserCacheDir() // %LocalAppData%
	if err != nil {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "tg-agent")
	_ = os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, "bot-vault")
}

const vaultLockWait = 30 * time.Second

// botCredTarget — имя записи токена в диспетчере учётных данных.
func botCredTarget(key string) string { return "tg-agent/bot/" + key }

// credSet, credGet — диспетчер учётных данных (в тестах — подделка).
var (
	credSet = wincred.Set
	credGet = wincred.Get
)

// loadVault — сведения о ботах (без токенов: они в диспетчере). Вызывать
// под vaultMu.
func loadVault() map[string]*BotEntry {
	db, err := vaultDB()
	if err != nil {
		return map[string]*BotEntry{}
	}
	out, err := botsTab.All(context.Background(), db.Q())
	if err != nil {
		return map[string]*BotEntry{}
	}
	return out
}

// vaultToken — токен бота по ключу (username в нижнем регистре).
func vaultToken(key string) (string, bool) {
	tok, err := credGet(botCredTarget(key))
	return tok, err == nil && tok != ""
}

// editBot — прочитать, поменять и записать сведения о боте key одной
// транзакцией. fn получает запись (nil — её нет) и возвращает новую (nil —
// ничего не писать).
func editBot(key string, fn func(e *BotEntry) *BotEntry) error {
	db, err := vaultDB()
	if err != nil {
		return err
	}
	ctx := context.Background()
	return db.Write(ctx, func(tx *sql.Tx) error {
		e, ok, err := botsTab.Get(ctx, tx, key)
		if err != nil {
			return err
		}
		if !ok {
			e = nil
		}
		next := fn(e)
		if next == nil {
			return nil
		}
		cp := *next
		cp.Token = "" // секрет — только в диспетчере
		return botsTab.Put(ctx, tx, key, &cp)
	})
}

// vaultHas — у бота с этим id в хранилище ровно этот токен.
func vaultHas(botID, tok string) bool {
	vaultMu.Lock()
	defer vaultMu.Unlock()
	for key, e := range loadVault() {
		if e.BotID == botID {
			cur, ok := vaultToken(key)
			return ok && cur == tok
		}
	}
	return false
}

type botMe struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

// botGetMe — getMe с этим токеном (в тестах — подделка).
var botGetMe = func(ctx context.Context, s *config.Settings, tok string) (botMe, error) {
	cp := *s
	cp.BotToken = tok // bot.scrub вычистит именно его из ошибок
	var me botMe
	err := bot.Call(ctx, &cp, "getMe", nil, &me)
	return me, err
}

type botTokenDead struct{ msg string }

func (e *botTokenDead) Error() string { return e.msg }

// saveBotToken — проверить токен (getMe) и положить в хранилище. Вернёт @username.
func saveBotToken(ctx context.Context, s *config.Settings, tok, source string) (string, error) {
	me, err := botGetMe(ctx, s, tok)
	if err != nil {
		if strings.Contains(err.Error(), "Unauthorized") || strings.Contains(err.Error(), "Not Found") {
			return "", &botTokenDead{"токен не действует (getMe: Unauthorized)"}
		}
		return "", fmt.Errorf("getMe: %w", err)
	}
	vaultMu.Lock()
	defer vaultMu.Unlock()
	key := strings.ToLower(me.Username)
	var cur string
	err = lock.With(ctx, vaultLockPath(), vaultLockWait, func() error {
		cur, _ = vaultToken(key)
		if err := credSet(botCredTarget(key), tok); err != nil {
			return fmt.Errorf("токен не записан в диспетчер учётных данных Windows: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	username := "@" + me.Username
	err = editBot(key, func(e *BotEntry) *BotEntry {
		if e == nil {
			e = &BotEntry{}
		}
		e.Username, e.BotID, e.Name = username, strconv.FormatInt(me.ID, 10), me.FirstName
		if cur != tok {
			e.Env = nil // старый токен в тех файлах больше не действует
		}
		e.SavedAt, e.Source = time.Now().Format(time.RFC3339), source
		return e
	})
	if err != nil {
		return "", err
	}
	logEvent(s, "bot_token_saved", "bot", username, "source", source)
	return username, nil
}

func noteEnv(username, where string) {
	vaultMu.Lock()
	defer vaultMu.Unlock()
	_ = editBot(strings.ToLower(strings.TrimPrefix(username, "@")), func(e *BotEntry) *BotEntry {
		if e != nil {
			e.Env = append(e.Env, where)
		}
		return e
	})
}

// VaultToken — токен из хранилища по @username (для tg bot-token).
func VaultToken(username string) (string, bool) {
	vaultMu.Lock()
	defer vaultMu.Unlock()
	key := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(username), "@"))
	if loadVault()[key] == nil {
		return "", false
	}
	return vaultToken(key)
}

// ListBots — tg_bots: боты в хранилище, без токенов.
func ListBots(s *config.Settings) (*omap.Map, error) {
	if s.Agent != nil {
		return nil, denied("Хранилище токенов ботов агенту из контейнера недоступно.")
	}
	vaultMu.Lock()
	v := loadVault()
	vaultMu.Unlock()
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	list := []*omap.Map{}
	for _, k := range keys {
		e := v[k]
		item := omap.New().Set("bot", e.Username).Set("bot_id", e.BotID).Set("name", e.Name).
			Set("saved_at", e.SavedAt).Set("source", e.Source)
		if len(e.Env) > 0 {
			item.Set("also_in", e.Env)
		}
		list = append(list, item)
	}
	return omap.New().Set("bots", list).Set("hint",
		"Токены не показываются. Взять — только трубой: bin\\tg.exe bot-token @бот | … ; "+
			"нового бота или перевыпуск — tg_botfather."), nil
}

// writeEnv — KEY=значение в .env: заменить строку или дописать.
func writeEnv(path, key, value string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if text == "" {
		lines = nil
	}
	done := false
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), key+"=") {
			lines[i] = key + "=" + value
			done = true
		}
	}
	if !done {
		lines = append(lines, key+"="+value)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsx.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

// ── итог действия для tg_wait_approval ───────────────────────────────────

// botOpsTab — итоги действий в BotFather по id черновика.
var botOpsTab = store.NewTable[json.RawMessage](store.TBotOps)

func saveBotOp(s *config.Settings, draftID string, r *omap.Map) {
	b, err := r.MarshalJSON()
	if err != nil {
		return
	}
	db, err := stateDB(s)
	if err != nil {
		logEvent(s, "bot_op_save_failed", "draft_id", draftID, "error", err.Error())
		return
	}
	err = botOpsTab.Edit(context.Background(), db, func(all map[string]json.RawMessage) bool {
		all[draftID] = secretSafe(b)
		if len(all) > 100 { // id растут со временем: самые старые — по ключу
			keys := make([]string, 0, len(all))
			for k := range all {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys[:len(all)-100] {
				delete(all, k)
			}
		}
		return true
	})
	if err != nil {
		logEvent(s, "bot_op_save_failed", "draft_id", draftID, "error", err.Error())
	}
}

func secretSafe(b []byte) json.RawMessage { return json.RawMessage(secret.Mask(string(b))) }

// botOpResult — что вышло у действия в BotFather (nil — не было).
func botOpResult(s *config.Settings, draftID string) json.RawMessage {
	db, err := stateDB(s)
	if err != nil {
		return nil
	}
	r, _, _ := botOpsTab.Get(context.Background(), db.Q(), draftID)
	return r
}
