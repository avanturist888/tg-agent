package config

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"tgagent/internal/store"
)

func TestAccounts(t *testing.T) {
	root := tempInstall(t)
	write(t, filepath.Join(root, ".env"), "TG_API_ID=1\nTG_API_HASH=h\nTG_BOT_TOKEN=main-bot\nTG_APPROVAL_CHAT_ID=42\n")
	write(t, filepath.Join(root, "config", "chats.toml"), "[[chat]]\nalias = \"team\"\nid = 1\n")
	if got := Accounts(); !slices.Equal(got, []string{"main"}) || Multi() {
		t.Fatalf("один аккаунт: %v", got)
	}
	// заготовка без .env — ещё не аккаунт
	write(t, filepath.Join(root, "profiles", "draft", "config", "chats.toml"), "")
	write(t, filepath.Join(root, "profiles", "work", ".env"), "TG_API_ID=1\nTG_API_HASH=h\n")
	write(t, filepath.Join(root, "profiles", "work", "config", "chats.toml"), "[[chat]]\nalias = \"team\"\nid = 2\n")
	write(t, filepath.Join(root, "profiles", "own", ".env"), "TG_BOT_TOKEN=own-bot\nTG_APPROVAL_CHAT_ID=42\n")
	if got := Accounts(); !slices.Equal(got, []string{"main", "own", "work"}) || !Multi() {
		t.Fatalf("аккаунты: %v", got)
	}
	if !SharesMainBot("work") || SharesMainBot("own") || SharesMainBot("main") {
		t.Fatal("общий бот — только у профиля без своего")
	}

	if a, alias, ok := SplitRef("Work/team"); !ok || a != "work" || alias != "team" {
		t.Fatalf("ссылка с аккаунтом: %q %q %v", a, alias, ok)
	}
	if _, _, ok := SplitRef("nobody/team"); ok {
		t.Fatal("неизвестный аккаунт — не ссылка")
	}
	if a, rest := SplitCallback("@work|d:1:ok"); a != "work" || rest != "d:1:ok" {
		t.Fatalf("метка кнопки: %q %q", a, rest)
	}
	if a, rest := SplitCallback("d:1:ok"); a != "" || rest != "d:1:ok" {
		t.Fatal("кнопка без метки")
	}

	// основной: alias как есть, «main/…» — тоже свой
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.Ref("team") != "team" || AccountName() != "main" {
		t.Fatal("у основного alias без аккаунта")
	}
	if r, err := s.Resolve("main/team"); err != nil || r.Alias != "team" {
		t.Fatalf("main/team: %v", err)
	}
	if _, err := s.Resolve("work/team"); err == nil {
		t.Fatal("чужой чат свой список не отдаёт")
	}

	// профиль: ссылки с его именем
	if err := SetProfile("work"); err != nil {
		t.Fatal(err)
	}
	s, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.Ref("team") != "work/team" || !IsOwnAccount("WORK") || IsOwnAccount("main") {
		t.Fatalf("ссылка профиля: %s", s.Ref("team"))
	}
	for _, ref := range []string{"team", "work/team"} {
		if r, err := s.Resolve(ref); err != nil || r.PeerString() != "2" {
			t.Fatalf("%s: %v", ref, err)
		}
	}
	if err := SetProfile("main"); err != nil || Profile() != "" {
		t.Fatal("main — это основной профиль")
	}
}

func putAccountMeta(t *testing.T, account string, meta AccountMeta) {
	t.Helper()
	db, err := store.Open(filepath.Join(HomeOf(account), "data"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := db.Write(ctx, func(tx *sql.Tx) error {
		return store.PutKV(ctx, tx, store.KeyAccount, meta)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNormPhone(t *testing.T) {
	cases := [][2]string{
		{"+7 (999) 123-45-67", "79991234567"},
		{"  8-800-555-35-35 ", "88005553535"},
		{"+1.234.5678", "12345678"},
		{"abc+7١٢٣99", "799"}, // не-ASCII цифры отбрасываются
		{"", ""},
	}
	for _, tc := range cases {
		if got := NormPhone(tc[0]); got != tc[1] {
			t.Errorf("NormPhone(%q) = %q, want %q", tc[0], got, tc[1])
		}
	}
}

func TestBotRoutingByPhoneAndSelfID(t *testing.T) {
	root := tempInstall(t)
	// main: Telegram, номер 1, общий бот, chat_id = 100
	write(t, filepath.Join(root, ".env"),
		"TG_API_ID=1\nTG_API_HASH=h\nTG_BOT_TOKEN=1:main\nTG_APPROVAL_CHAT_ID=100\nTG_PHONE=+7 (999) 111-11-11\n")
	write(t, filepath.Join(root, "config", "chats.toml"), "[[chat]]\nalias = \"saved\"\nid = \"me\"\n")

	// work: Telegram, номер 2 (только в state.db), SelfID = 200, своего бота нет
	write(t, filepath.Join(root, "profiles", "work", ".env"), "TG_API_ID=1\nTG_API_HASH=h\n")
	write(t, filepath.Join(root, "profiles", "work", "config", "chats.toml"), "[[chat]]\nalias = \"saved\"\nid = \"me\"\n")
	putAccountMeta(t, "work", AccountMeta{ID: 200, Phone: "79992222222", FirstName: "Work"})

	// work-wa: WhatsApp с номером work (в state.db) -> должен пойти в main-бот и chat_id 200
	write(t, filepath.Join(root, "profiles", "work-wa", ".env"), "TG_NETWORK=whatsapp\n")
	write(t, filepath.Join(root, "profiles", "work-wa", "config", "chats.toml"), "[[chat]]\nalias = \"c\"\nid = 1\n")
	putAccountMeta(t, "work-wa", AccountMeta{Phone: "79992222222", FirstName: "WorkWA"})

	// work-max: MAX с номером work (в TG_PHONE .env) -> должен пойти в main-бот и chat_id 200
	write(t, filepath.Join(root, "profiles", "work-max", ".env"), "TG_NETWORK=max\nTG_PHONE=+7-999-222-22-22\n")
	write(t, filepath.Join(root, "profiles", "work-max", "config", "chats.toml"), "[[chat]]\nalias = \"c\"\nid = 1\n")

	// main-wa: WhatsApp с номером main -> main-бот и chat_id 100
	write(t, filepath.Join(root, "profiles", "main-wa", ".env"), "TG_NETWORK=wa\nTG_PHONE=+7 999 111 11 11\n")
	write(t, filepath.Join(root, "profiles", "main-wa", "config", "chats.toml"), "[[chat]]\nalias = \"c\"\nid = 1\n")

	// orphan-max: MAX без совпадения номера -> fallback на main-бот и chat_id 100
	write(t, filepath.Join(root, "profiles", "orphan-max", ".env"), "TG_NETWORK=max\nTG_PHONE=+79990000000\n")
	write(t, filepath.Join(root, "profiles", "orphan-max", "config", "chats.toml"), "[[chat]]\nalias = \"c\"\nid = 1\n")

	// explicit: Telegram с явным TG_APPROVAL_CHAT_ID=555 и без бота -> 555 НЕ затирается mainChat (100)
	write(t, filepath.Join(root, "profiles", "explicit", ".env"),
		"TG_API_ID=1\nTG_API_HASH=h\nTG_APPROVAL_CHAT_ID=555\n")
	write(t, filepath.Join(root, "profiles", "explicit", "config", "chats.toml"), "[[chat]]\nalias = \"c\"\nid = 1\n")
	putAccountMeta(t, "explicit", AccountMeta{ID: 666, Phone: "79996666666"})

	// nostate: профиль без state.db — чтение конфига НЕ должно создавать state.db на диске
	write(t, filepath.Join(root, "profiles", "nostate", ".env"), "TG_API_ID=1\nTG_API_HASH=h\n")
	write(t, filepath.Join(root, "profiles", "nostate", "config", "chats.toml"), "[[chat]]\nalias = \"c\"\nid = 1\n")

	// Проверяем AccountNetwork / AccountPhone / AccountSelfID
	if AccountNetwork("main") != NetTelegram || AccountNetwork("work-wa") != NetWA ||
		AccountNetwork("main-wa") != NetWA || AccountNetwork("work-max") != NetMAX {
		t.Fatalf("сети: main=%s work-wa=%s main-wa=%s work-max=%s",
			AccountNetwork("main"), AccountNetwork("work-wa"), AccountNetwork("main-wa"), AccountNetwork("work-max"))
	}
	if AccountPhone("main") != "79991111111" || AccountPhone("work") != "79992222222" || AccountSelfID("work") != 200 {
		t.Fatalf("phone/selfID: main=%s work=%s/%d", AccountPhone("main"), AccountPhone("work"), AccountSelfID("work"))
	}

	// Проверяем BotOf и ApprovalChatIDOf для всех профилей (без Load чужих профилей)
	want := []struct {
		acct   string
		tok    string
		chat   int64
		shared bool
	}{
		{"main", "1:main", 100, false},
		{"work", "1:main", 200, true},
		{"work-wa", "1:main", 200, true},
		{"work-max", "1:main", 200, true},
		{"main-wa", "1:main", 100, true},
		{"orphan-max", "1:main", 100, true},
		{"explicit", "1:main", 555, true},
		{"nostate", "1:main", 100, true},
	}
	for _, tc := range want {
		tok, chat, shared := BotOf(tc.acct)
		if tok != tc.tok || chat != tc.chat || shared != tc.shared {
			t.Errorf("BotOf(%s) = (%q, %d, %v), want (%q, %d, %v)", tc.acct, tok, chat, shared, tc.tok, tc.chat, tc.shared)
		}
		if ApprovalChatIDOf(tc.acct) != tc.chat {
			t.Errorf("ApprovalChatIDOf(%s) = %d, want %d", tc.acct, ApprovalChatIDOf(tc.acct), tc.chat)
		}
	}

	// SharesBotWith
	if !SharesBotWith("work", "main") || !SharesBotWith("work-wa", "main") || !SharesMainBot("work-max") {
		t.Fatal("work, work-wa и work-max должны делить бот с main")
	}
	if SharesBotWith("work-wa", "work") {
		t.Fatal("work сам делит бота (shared=true), он не хост бота")
	}
	if SharesBotWith("main", "main") {
		t.Fatal("аккаунт не делит бота с самим собой")
	}

	// Чтение чужих профилей (BotOf, AccountPhone, AccountSelfID, SharesBotWith) и Load(main)
	// не должны создавать пустые state.db на диске у профилей без базы
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	for _, acct := range []string{"nostate", "work-max", "main-wa", "orphan-max"} {
		dbPath := filepath.Join(HomeOf(acct), "data", store.FileName)
		if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("чтение конфига создало лишний %s (err=%v)", dbPath, err)
		}
	}

	// Теперь проверяем Load() для каждого профиля
	for _, tc := range want {
		if err := SetProfile(tc.acct); err != nil {
			t.Fatal(err)
		}
		s, err := Load()
		if err != nil {
			t.Fatalf("Load(%s): %v", tc.acct, err)
		}
		if s.BotToken != tc.tok || s.ApprovalChatID != tc.chat || s.SharedBot != tc.shared {
			t.Errorf("Load(%s) = (%q, %d, %v), want (%q, %d, %v)", tc.acct, s.BotToken, s.ApprovalChatID, s.SharedBot, tc.tok, tc.chat, tc.shared)
		}
	}
	_ = SetProfile("")
}

func TestOwnBotInheritedBySamePhone(t *testing.T) {
	root := tempInstall(t)
	write(t, filepath.Join(root, ".env"),
		"TG_API_ID=1\nTG_API_HASH=h\nTG_BOT_TOKEN=1:main\nTG_APPROVAL_CHAT_ID=100\nTG_PHONE=+79991111111\n")
	write(t, filepath.Join(root, "config", "chats.toml"), "[[chat]]\nalias = \"c\"\nid = 1\n")

	// work имеет своего отдельного бота work-bot и номер 79992222222
	write(t, filepath.Join(root, "profiles", "work", ".env"),
		"TG_API_ID=1\nTG_API_HASH=h\nTG_BOT_TOKEN=2:work\nTG_PHONE=+79992222222\n")
	write(t, filepath.Join(root, "profiles", "work", "config", "chats.toml"), "[[chat]]\nalias = \"c\"\nid = 1\n")
	putAccountMeta(t, "work", AccountMeta{ID: 200, Phone: "79992222222"})

	// work-wa без своего бота, тот же номер -> наследует 2:work и chat_id 200 с shared=true
	write(t, filepath.Join(root, "profiles", "work-wa", ".env"), "TG_NETWORK=whatsapp\nTG_PHONE=+79992222222\n")
	write(t, filepath.Join(root, "profiles", "work-wa", "config", "chats.toml"), "[[chat]]\nalias = \"c\"\nid = 1\n")

	// work-max с явным TG_APPROVAL_CHAT_ID=205 -> наследует 2:work (shared=true), но сохраняет свой 205
	write(t, filepath.Join(root, "profiles", "work-max", ".env"),
		"TG_NETWORK=max\nTG_PHONE=+79992222222\nTG_APPROVAL_CHAT_ID=205\n")
	write(t, filepath.Join(root, "profiles", "work-max", "config", "chats.toml"), "[[chat]]\nalias = \"c\"\nid = 1\n")

	// disabled-wa с TG_SEND_POLICY=human_approval не делит бота
	write(t, filepath.Join(root, "profiles", "disabled-wa", ".env"),
		"TG_NETWORK=whatsapp\nTG_PHONE=+79992222222\nTG_SEND_POLICY=human_approval\n")
	write(t, filepath.Join(root, "profiles", "disabled-wa", "config", "chats.toml"), "[[chat]]\nalias = \"c\"\nid = 1\n")

	if tok, chat, shared := BotOf("work"); tok != "2:work" || chat != 200 || shared {
		t.Fatalf("work: (%q, %d, %v)", tok, chat, shared)
	}
	if tok, chat, shared := BotOf("work-wa"); tok != "2:work" || chat != 200 || !shared {
		t.Fatalf("work-wa: (%q, %d, %v)", tok, chat, shared)
	}
	if tok, chat, shared := BotOf("work-max"); tok != "2:work" || chat != 205 || !shared {
		t.Fatalf("work-max: (%q, %d, %v)", tok, chat, shared)
	}
	if !SharesBotWith("work-wa", "work") || !SharesBotWith("work-max", "work") {
		t.Fatal("work-wa и work-max должны делить бота с work")
	}
	if SharesBotWith("work-wa", "main") || SharesMainBot("work-wa") {
		t.Fatal("work-wa не должен считаться общим с main")
	}
	if SharesBotWith("disabled-wa", "work") {
		t.Fatal("профиль с human_approval не делит бота")
	}
	if err := SetProfile("work-wa"); err != nil {
		t.Fatal(err)
	}
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.BotToken != "2:work" || s.ApprovalChatID != 200 || !s.SharedBot || s.CallbackPrefix() != "@work-wa|" {
		t.Fatalf("Load(work-wa): %+v", s)
	}
}
