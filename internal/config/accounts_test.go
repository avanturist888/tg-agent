package config

import (
	"path/filepath"
	"slices"
	"testing"
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
