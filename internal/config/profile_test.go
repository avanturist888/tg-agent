package config

import (
	"os"
	"path/filepath"
	"testing"
)

// tempInstall — установка во временной папке; профиль сбрасывается после теста.
func tempInstall(t *testing.T) string {
	t.Helper()
	old := Root
	Root = t.TempDir()
	t.Cleanup(func() {
		Root = old
		_ = SetProfile("")
	})
	return Root
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProfilePaths(t *testing.T) {
	root := tempInstall(t)
	if err := SetProfile(""); err != nil {
		t.Fatal(err)
	}
	if Home() != root || DataPath() != filepath.Join(root, "data") || ChatsFile() != filepath.Join(root, "config", "chats.toml") ||
		EnvPath() != filepath.Join(root, ".env") {
		t.Fatalf("основной профиль должен жить в корне: %s %s %s", Home(), DataPath(), ChatsFile())
	}
	if err := SetProfile("work"); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "profiles", "work")
	if Home() != home || DataPath() != filepath.Join(home, "data") || ChatsFile() != filepath.Join(home, "config", "chats.toml") ||
		EnvPath() != filepath.Join(home, ".env") || T3Path() != filepath.Join(home, "config", "t3.toml") ||
		AgentsPath() != filepath.Join(home, "config", "agents.toml") {
		t.Fatalf("пути профиля: %s %s %s", Home(), DataPath(), ChatsFile())
	}
	if os.Getenv(ProfileEnv) != "work" {
		t.Fatal("профиль должен уходить в окружение дочерних процессов")
	}
	if err := SetProfile("../evil"); err == nil {
		t.Fatal("имя профиля с путём")
	}
	if err := SetProfile("default"); err != nil || Profile() != "" {
		t.Fatal("default — это основной профиль")
	}
}

func TestProfileOwnSettingsAndBot(t *testing.T) {
	root := tempInstall(t)
	// основной профиль с ботом
	write(t, filepath.Join(root, ".env"), "TG_API_ID=1\nTG_API_HASH=main\nTG_BOT_TOKEN=main-bot\nTG_APPROVAL_CHAT_ID=42\nTG_PHONE=+7000\n")
	write(t, filepath.Join(root, "config", "chats.toml"), "[[chat]]\nalias = \"personal\"\nid = 1\n")
	home := filepath.Join(root, "profiles", "work")
	write(t, filepath.Join(home, "config", "chats.toml"), "[[chat]]\nalias = \"saved\"\nid = \"me\"\n")
	write(t, filepath.Join(home, ".env"), "TG_API_ID=1\nTG_API_HASH=main\nTG_SEND_POLICY=human_approval\n")
	if err := SetProfile("work"); err != nil {
		t.Fatal(err)
	}
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Chats["saved"]; !ok || len(s.Chats) != 1 {
		t.Fatalf("у профиля свой белый список: %v", s.ChatOrder)
	}
	if s.SessionPath != filepath.Join(home, "data", "session.json") || s.AuditPath() != filepath.Join(home, "data", "audit.jsonl") {
		t.Fatalf("сессия и аудит профиля: %s %s", s.SessionPath, s.AuditPath())
	}
	if s.BotToken != "" || s.Phone != "" {
		t.Fatalf("бот и номер основного профиля в профиль не попадают: бот %q, номер %q", s.BotToken, s.Phone)
	}

	// без своего бота при bot_approval — бот основного, нажатия через основную службу
	write(t, filepath.Join(home, ".env"), "TG_API_ID=1\nTG_API_HASH=main\n")
	if s, err := Load(); err != nil || !s.SharedBot || s.BotToken != "main-bot" || s.ApprovalChatID != 42 ||
		s.CallbackPrefix() != "@work|" {
		t.Fatalf("общий бот: %v %+v", err, s)
	}
	// тот же токен явно — тоже общий бот, а не второй слушатель
	write(t, filepath.Join(home, ".env"), "TG_API_ID=1\nTG_API_HASH=main\nTG_BOT_TOKEN=main-bot\nTG_APPROVAL_CHAT_ID=42\n")
	if s, err := Load(); err != nil || !s.SharedBot {
		t.Fatalf("тот же бот: %v", err)
	}
	// свой бот — можно
	write(t, filepath.Join(home, ".env"), "TG_API_ID=1\nTG_API_HASH=main\nTG_BOT_TOKEN=work-bot\nTG_APPROVAL_CHAT_ID=42\n")
	if s, err := Load(); err != nil || s.BotToken != "work-bot" || s.SharedBot || s.CallbackPrefix() != "" {
		t.Fatalf("свой бот профиля: %v", err)
	}

	// основной профиль не изменился
	if err := SetProfile(""); err != nil {
		t.Fatal(err)
	}
	s, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Chats["personal"]; !ok || s.BotToken != "main-bot" || s.SharedBot || s.SessionPath != filepath.Join(root, "data", "session.json") {
		t.Fatalf("основной профиль: чаты %v, бот %q, сессия %s", s.ChatOrder, s.BotToken, s.SessionPath)
	}
}
