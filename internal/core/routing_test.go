package core

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"tgagent/internal/bot"
	"tgagent/internal/config"
	"tgagent/internal/maxc"
	"tgagent/internal/outbox"
	"tgagent/internal/store"
)

type recordedBotCall struct {
	method  string
	payload map[string]any
}

type recordedAnswer struct {
	id   string
	text string
}

type routingEnv struct {
	mainSettings *config.Settings
	botCalls     []recordedBotCall
	answers      []recordedAnswer
	passed       []struct {
		acct string
		data string
	}
}

func setupRoutingEnv(t *testing.T) *routingEnv {
	t.Helper()
	oldRoot := config.Root
	config.Root = t.TempDir()
	t.Cleanup(func() {
		config.Root = oldRoot
		_ = config.SetProfile("")
		pendingNote.Lock()
		pendingNote.byChat = nil
		pendingNote.Unlock()
	})
	write := func(rel, body string) {
		path := filepath.Join(config.Root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	putMeta := func(acct string, meta config.AccountMeta) {
		db, err := store.Open(filepath.Join(config.HomeOf(acct), "data"))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Write(context.Background(), func(tx *sql.Tx) error {
			return store.PutKV(context.Background(), tx, store.KeyAccount, meta)
		}); err != nil {
			t.Fatal(err)
		}
	}

	// main: chat_id=100, bot=1:main, phone=79991111111
	write(".env", "TG_API_ID=1\nTG_API_HASH=x\nTG_BOT_TOKEN=1:main\nTG_APPROVAL_CHAT_ID=100\nTG_PHONE=+79991111111\n")
	write("config/chats.toml", "[[chat]]\nalias = \"team\"\nid = -100123\nsend = true\ntitle = \"Команда\"\n")

	// work: Telegram, shares 1:main, SelfID=200, phone=79992222222
	write("profiles/work/.env", "TG_API_ID=1\nTG_API_HASH=x\nTG_PHONE=+79992222222\n")
	write("profiles/work/config/chats.toml", "[[chat]]\nalias = \"client\"\nid = -100777\nsend = true\ntitle = \"Клиент\"\n")
	putMeta("work", config.AccountMeta{ID: 200, Phone: "79992222222", FirstName: "Work"})

	// work-wa: WhatsApp with work's phone -> shares 1:main, chat_id=200
	write("profiles/work-wa/.env", "TG_NETWORK=whatsapp\n")
	write("profiles/work-wa/config/chats.toml", "[[chat]]\nalias = \"boss\"\nid = 55\nsend = true\ntitle = \"Босс\"\n")
	putMeta("work-wa", config.AccountMeta{Phone: "79992222222", FirstName: "WorkWA"})

	// own: separate bot 2:own, chat_id=300
	write("profiles/own/.env", "TG_API_ID=1\nTG_API_HASH=x\nTG_BOT_TOKEN=2:own\nTG_APPROVAL_CHAT_ID=300\n")
	write("profiles/own/config/chats.toml", "[[chat]]\nalias = \"solo\"\nid = -100999\nsend = true\ntitle = \"Соло\"\n")

	env := &routingEnv{}
	oldBotCall := botCall
	oldAnswer := answerCallback
	oldPass := callAccountCallback
	botCall = func(_ context.Context, _ *config.Settings, method string, payload any, _ any) error {
		m, _ := payload.(map[string]any)
		env.botCalls = append(env.botCalls, recordedBotCall{method: method, payload: m})
		return nil
	}
	answerCallback = func(_ context.Context, _ *config.Settings, id, text string) {
		env.answers = append(env.answers, recordedAnswer{id: id, text: text})
	}
	callAccountCallback = func(_ context.Context, acct string, q *bot.CallbackQuery) error {
		env.passed = append(env.passed, struct {
			acct string
			data string
		}{acct: acct, data: q.Data})
		return nil
	}
	t.Cleanup(func() {
		botCall = oldBotCall
		answerCallback = oldAnswer
		callAccountCallback = oldPass
	})

	if err := config.SetProfile(""); err != nil {
		t.Fatal(err)
	}
	s, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	env.mainSettings = s
	return env
}

func makeCB(fromID int64, data string, msgID int64) *bot.CallbackQuery {
	q := &bot.CallbackQuery{ID: "cb1", Data: data}
	q.From.ID = fromID
	if msgID != 0 {
		q.Message = &struct {
			MessageID int64 `json:"message_id"`
		}{MessageID: msgID}
	}
	return q
}

func makeMsg(fromID, chatID int64, text string, isBot bool) *bot.Message {
	m := &bot.Message{MessageID: 1, Text: text}
	m.From.ID = fromID
	m.From.IsBot = isBot
	m.Chat.ID = chatID
	return m
}

func TestHandleCallbackRoutingAndSecurity(t *testing.T) {
	env := setupRoutingEnv(t)
	s := env.mainSettings
	ctx := context.Background()

	// 1. @work|d:abc:ok от workChatID (200) -> пересылается work
	if err := HandleCallback(ctx, s, makeCB(200, "@work|d:abc:ok", 10)); err != nil {
		t.Fatal(err)
	}
	// 2. @work-wa|d:xyz:ok от workChatID (200) -> пересылается work-wa
	if err := HandleCallback(ctx, s, makeCB(200, "@work-wa|d:xyz:ok", 11)); err != nil {
		t.Fatal(err)
	}
	if len(env.passed) != 2 || env.passed[0].acct != "work" || env.passed[1].acct != "work-wa" {
		t.Fatalf("пересылка для work/work-wa: %+v", env.passed)
	}

	// 3. @work|d:abc:ok от mainChatID (100) и от постороннего (999) -> отказ, без пересылки
	env.passed = nil
	env.answers = nil
	for _, sender := range []int64{100, 999} {
		if err := HandleCallback(ctx, s, makeCB(sender, "@work|d:abc:ok", 10)); err != nil {
			t.Fatal(err)
		}
	}
	if len(env.passed) != 0 {
		t.Fatalf("чужие не должны пересылаться: %+v", env.passed)
	}
	if len(env.answers) != 2 || env.answers[0].text != "Эта кнопка не для вас." || env.answers[1].text != "Эта кнопка не для вас." {
		t.Fatalf("ответы при отказе: %+v", env.answers)
	}

	// 4. @own|d:abc:ok от 300 (own НЕ делит бота main!) -> отказ
	env.answers = nil
	if err := HandleCallback(ctx, s, makeCB(300, "@own|d:abc:ok", 10)); err != nil {
		t.Fatal(err)
	}
	if len(env.passed) != 0 || len(env.answers) != 1 || env.answers[0].text != "Эта кнопка не для вас." {
		t.Fatalf("аккаунт со своим ботом не пересылается: passed=%+v answers=%+v", env.passed, env.answers)
	}

	// 5. Несуществующий аккаунт
	env.answers = nil
	if err := HandleCallback(ctx, s, makeCB(200, "@ghost|d:abc:ok", 10)); err != nil {
		t.Fatal(err)
	}
	if len(env.answers) != 1 || !strings.Contains(env.answers[0].text, "больше нет") {
		t.Fatalf("несуществующий аккаунт: %+v", env.answers)
	}

	// 6. Кнопки main без префикса (d:, a:, s:, u:ack) от workChatID (200) -> отказ
	draft, err := outbox.New(s.DataDir()).Create(outbox.CreateOpts{Chat: "team", Text: "hi", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	env.answers = nil
	for _, data := range []string{"d:" + draft.ID + ":ok", "a:req1:r", "s:team~a0", upstreamAck} {
		if err := HandleCallback(ctx, s, makeCB(200, data, 10)); err != nil {
			t.Fatal(err)
		}
	}
	if len(env.answers) != 4 {
		t.Fatalf("ожидали 4 отказа, получили %+v", env.answers)
	}
	for _, a := range env.answers {
		if a.text != "Эта кнопка не для вас." {
			t.Fatalf("неверный ответ отказа: %+v", a)
		}
	}
	gotDraft, err := outbox.New(s.DataDir()).Get(draft.ID)
	if err != nil || gotDraft.Status != outbox.Pending {
		t.Fatalf("черновик main не должен был сменить статус: %+v %v", gotDraft, err)
	}
}

func TestHandleMessageAndAutoMenuIsolation(t *testing.T) {
	env := setupRoutingEnv(t)
	s := env.mainSettings
	ctx := context.Background()

	// /start от workChatID (200) -> setMyCommands + ответ в 200, только work и work-wa, без /updates
	HandleMessage(ctx, s, makeMsg(200, 200, "/start", false))
	if len(env.botCalls) != 2 || env.botCalls[0].method != "setMyCommands" ||
		env.botCalls[1].method != "sendMessage" || env.botCalls[1].payload["chat_id"] != int64(200) {
		t.Fatalf("/start для work: %+v", env.botCalls)
	}
	startText := env.botCalls[1].payload["text"].(string)
	if !strings.Contains(startText, "work, work-wa") || strings.Contains(startText, "main") || strings.Contains(startText, "/updates") {
		t.Fatalf("текст /start для work:\n%s", startText)
	}

	// /start от mainChatID (100) -> setMyCommands + ответ в 100, только main, с /updates
	env.botCalls = nil
	HandleMessage(ctx, s, makeMsg(100, 100, "/start", false))
	if len(env.botCalls) != 2 || env.botCalls[0].method != "setMyCommands" ||
		env.botCalls[1].method != "sendMessage" || env.botCalls[1].payload["chat_id"] != int64(100) {
		t.Fatalf("/start для main: %+v", env.botCalls)
	}
	mainStart := env.botCalls[1].payload["text"].(string)
	if !strings.Contains(mainStart, "<b>main</b>") || strings.Contains(mainStart, "work") || !strings.Contains(mainStart, "/updates") {
		t.Fatalf("текст /start для main:\n%s", mainStart)
	}

	// /auto от workChatID (200) -> меню в 200 только с work и work-wa
	env.botCalls = nil
	HandleMessage(ctx, s, makeMsg(200, 200, "/auto", false))
	if len(env.botCalls) != 1 || env.botCalls[0].payload["chat_id"] != int64(200) {
		t.Fatalf("/auto для work: %+v", env.botCalls)
	}
	kb := env.botCalls[0].payload["reply_markup"].(map[string]any)["inline_keyboard"].([][]map[string]string)
	var cbData []string
	for _, row := range kb {
		for _, btn := range row {
			if !strings.HasPrefix(btn["callback_data"], "am:") {
				cbData = append(cbData, btn["callback_data"])
			}
		}
	}
	if !slices.Equal(cbData, []string{"@work|s:client~a0", "@work-wa|s:boss~a0"}) {
		t.Fatalf("кнопки /auto для work: %v", cbData)
	}

	// am:y0 от workChatID (200) -> перерисовывает в 200 только с аккаунтами work
	env.botCalls = nil
	if err := HandleCallback(ctx, s, makeCB(200, "am:n0", 77)); err != nil {
		t.Fatal(err)
	}
	if len(env.botCalls) != 1 || env.botCalls[0].method != "editMessageText" ||
		env.botCalls[0].payload["chat_id"] != int64(200) || env.botCalls[0].payload["message_id"] != int64(77) {
		t.Fatalf("am:n0 от work: %+v", env.botCalls)
	}

	// am:n0 от постороннего (999) -> отказ
	env.botCalls = nil
	env.answers = nil
	if err := HandleCallback(ctx, s, makeCB(999, "am:n0", 77)); err != nil {
		t.Fatal(err)
	}
	if len(env.botCalls) != 0 || len(env.answers) != 1 || env.answers[0].text != "Эта кнопка не для вас." {
		t.Fatalf("am:n0 от постороннего: calls=%+v answers=%+v", env.botCalls, env.answers)
	}

	// Групповое сообщение, посторонний, сервисное от бота
	env.botCalls = nil
	HandleMessage(ctx, s, makeMsg(200, -500, "/auto", false))
	HandleMessage(ctx, s, makeMsg(999, 999, "/auto", false))
	HandleMessage(ctx, s, makeMsg(555, 200, "pinned", true))
	if len(env.botCalls) != 0 {
		t.Fatalf("не должно быть ответов: %+v", env.botCalls)
	}

	// handleAutoCallback внутри профиля work перерисовывает меню в chat_id=200 только для work и work-wa
	if err := config.SetProfile("work"); err != nil {
		t.Fatal(err)
	}
	ws, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	env.botCalls = nil
	if err := HandleCallback(ctx, ws, makeCB(200, "@work|s:client~a0", 88)); err != nil {
		t.Fatal(err)
	}
	if len(env.botCalls) != 1 || env.botCalls[0].method != "editMessageText" || env.botCalls[0].payload["chat_id"] != int64(200) {
		t.Fatalf("editAutoMenu из work: %+v", env.botCalls)
	}
	wkb := env.botCalls[0].payload["reply_markup"].(map[string]any)["inline_keyboard"].([][]map[string]string)
	var wcb []string
	for _, row := range wkb {
		for _, btn := range row {
			if !strings.HasPrefix(btn["callback_data"], "am:") {
				wcb = append(wcb, btn["callback_data"])
			}
		}
	}
	if !slices.Equal(wcb, []string{"@work|s:client~a0", "@work-wa|s:boss~a0"}) {
		t.Fatalf("перерисованные кнопки из work: %v", wcb)
	}
}

func TestNotesIsolationBetweenChats(t *testing.T) {
	env := setupRoutingEnv(t)
	s := env.mainSettings
	ctx := context.Background()

	mainNote, err := AddNote("main", "личная заметка main", noteByOwner, "")
	if err != nil {
		t.Fatal(err)
	}
	workNote, err := AddNote("work", "рабочая заметка work", noteByOwner, "")
	if err != nil {
		t.Fatal(err)
	}

	// /notes от workChatID (200) показывает только work и work-wa
	HandleMessage(ctx, s, makeMsg(200, 200, "/notes", false))
	if len(env.botCalls) != 1 || env.botCalls[0].payload["chat_id"] != int64(200) {
		t.Fatalf("/notes от work: %+v", env.botCalls)
	}
	text := env.botCalls[0].payload["text"].(string)
	if !strings.Contains(text, "рабочая заметка work") || strings.Contains(text, "личная заметка main") || strings.Contains(text, "<b>main</b>") {
		t.Fatalf("утечка заметок в /notes work:\n%s", text)
	}

	// workChatID не может нажать n:+:main и не может удалить заметку main
	env.answers = nil
	env.botCalls = nil
	if err := HandleCallback(ctx, s, makeCB(200, "n:+:main", 50)); err != nil {
		t.Fatal(err)
	}
	if err := HandleCallback(ctx, s, makeCB(200, "n:-:"+mainNote.ID, 50)); err != nil {
		t.Fatal(err)
	}
	if len(env.answers) != 2 || env.answers[0].text != "Эта кнопка не для вас." || env.answers[1].text != "Эта кнопка не для вас." {
		t.Fatalf("отказы на чужие заметки: %+v", env.answers)
	}
	if len(NotesOf("main")) != 1 {
		t.Fatal("заметка main не должна была удалиться из чата work")
	}

	// mainChatID не может удалить заметку work
	env.answers = nil
	if err := HandleCallback(ctx, s, makeCB(100, "n:-:"+workNote.ID, 51)); err != nil {
		t.Fatal(err)
	}
	if len(env.answers) != 1 || env.answers[0].text != "Эта кнопка не для вас." || len(NotesOf("work")) != 1 {
		t.Fatalf("main не должен удалять заметку work: %+v", env.answers)
	}

	// Параллельный pendingNote: чат 100 и чат 200 не перебивают друг друга
	if err := HandleCallback(ctx, s, makeCB(100, "n:+:main", 60)); err != nil {
		t.Fatal(err)
	}
	if err := HandleCallback(ctx, s, makeCB(200, "n:+:work", 61)); err != nil {
		t.Fatal(err)
	}
	HandleMessage(ctx, s, makeMsg(100, 100, "вторая заметка main", false))
	HandleMessage(ctx, s, makeMsg(200, 200, "вторая заметка work", false))
	if len(NotesOf("main")) != 2 || NotesOf("main")[1].Text != "вторая заметка main" {
		t.Fatalf("pendingNote main: %+v", NotesOf("main"))
	}
	if len(NotesOf("work")) != 2 || NotesOf("work")[1].Text != "вторая заметка work" {
		t.Fatalf("pendingNote work: %+v", NotesOf("work"))
	}

	// /note main ... из workChatID запрещён, а /note work-wa ... работает
	env.botCalls = nil
	HandleMessage(ctx, s, makeMsg(200, 200, "/note main чужой текст", false))
	if len(NotesOf("main")) != 2 {
		t.Fatal("work не должен добавлять заметку в main через /note")
	}
	HandleMessage(ctx, s, makeMsg(200, 200, "/note work-wa заметка для wa", false))
	if got := NotesOf("work-wa"); len(got) != 1 || got[0].Text != "заметка для wa" {
		t.Fatalf("/note work-wa: %+v", got)
	}

	// Удаление своей заметки work из workChatID работает и перерисовывает меню в 200
	env.botCalls = nil
	if err := HandleCallback(ctx, s, makeCB(200, "n:-:"+workNote.ID, 62)); err != nil {
		t.Fatal(err)
	}
	if len(NotesOf("work")) != 1 || len(env.botCalls) != 1 || env.botCalls[0].method != "editMessageText" ||
		env.botCalls[0].payload["chat_id"] != int64(200) {
		t.Fatalf("удаление своей заметки work: notes=%d calls=%+v", len(NotesOf("work")), env.botCalls)
	}
}

func TestSetCommandsPerUniqueChatID(t *testing.T) {
	env := setupRoutingEnv(t)
	setCommands(context.Background(), env.mainSettings)

	got := map[int64][]string{}
	for _, c := range env.botCalls {
		if c.method != "setMyCommands" {
			continue
		}
		cid := c.payload["scope"].(map[string]any)["chat_id"].(int64)
		var names []string
		for _, cmd := range c.payload["commands"].([]map[string]string) {
			names = append(names, cmd["command"])
		}
		got[cid] = names
	}
	if len(got) != 2 {
		t.Fatalf("ожидали setMyCommands для 2 уникальных chat_id (100 и 200), получили %+v", got)
	}
	if !slices.Equal(got[100], []string{"auto", "notes", "updates"}) {
		t.Fatalf("команды для 100: %v", got[100])
	}
	if !slices.Equal(got[200], []string{"auto", "notes"}) {
		t.Fatalf("команды для 200: %v", got[200])
	}
	if _, leaked := got[300]; leaked {
		t.Fatal("chat_id 300 принадлежит другому боту own")
	}
}

func TestMaxLoggedInSavesAccountMeta(t *testing.T) {
	env := setupRoutingEnv(t)
	_ = env
	if err := config.SetProfile("work-wa"); err != nil {
		t.Fatal(err)
	}
	// Проверяем запись meta в kv.account при maxLoggedIn (на примере профиля work-max)
	write := func(rel, body string) {
		path := filepath.Join(config.Root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("profiles/work-max/.env", "TG_NETWORK=max\n")
	write("profiles/work-max/config/chats.toml", "[[chat]]\nalias = \"c\"\nid = 1\n")
	if err := config.SetProfile("work-max"); err != nil {
		t.Fatal(err)
	}
	r := &maxc.LoginResult{}
	r.Profile.Contact = maxc.Contact{
		ID:    4242,
		Phone: 79992222222,
		Names: []maxc.Name{{Name: "Иван MAX"}},
	}
	// loader возвращает ошибку, чтобы не запускать фоновый catchup
	maxLoggedIn(context.Background(), func() (*config.Settings, error) { return nil, os.ErrNotExist }, r)

	meta, ok := config.AccountMetaOf("work-max")
	if !ok || meta.ID != 4242 || meta.Phone != "79992222222" || meta.FirstName != "Иван MAX" {
		t.Fatalf("maxLoggedIn kv.account: ok=%v meta=%+v", ok, meta)
	}
	if got := config.ApprovalChatIDOf("work-max"); got != 200 {
		t.Fatalf("после сохранения Phone в MAX ApprovalChatIDOf = %d, want 200", got)
	}
}
