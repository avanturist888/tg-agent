package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tgagent/internal/config"
)

func TestAutoMenuToggle(t *testing.T) {
	old := config.Root
	config.Root = t.TempDir()
	t.Cleanup(func() { config.Root = old })
	os.MkdirAll(filepath.Join(config.Root, "config"), 0o700)
	os.WriteFile(config.ChatsFile(), []byte(`
[[chat]]
alias = "work"
id = -100123
title = "Работа"
read = true
send = true

[[chat]]
alias = "news"
id = -100456
title = "Новости"
read = true
send = false
`), 0o600)
	load := func() *config.Settings {
		rules, order, err := config.LoadChats(config.ChatsFile())
		if err != nil {
			t.Fatal(err)
		}
		return &config.Settings{Chats: rules, ChatOrder: order}
	}

	kb := autoKeyboard(load())["inline_keyboard"].([][]map[string]string)
	if len(kb) != 1 || kb[0][0]["callback_data"] != "s:work" || !strings.HasPrefix(kb[0][0]["text"], "⚪") {
		t.Fatalf("в меню только чат с отправкой, выключенный: %v", kb)
	}
	if on, err := toggleAuto(load(), "work"); err != nil || !on || !load().Chats["work"].AutoSend() {
		t.Fatalf("включение: %v %v", on, err)
	}
	kb = autoKeyboard(load())["inline_keyboard"].([][]map[string]string)
	if !strings.HasPrefix(kb[0][0]["text"], "🟢") {
		t.Fatalf("после включения: %v", kb)
	}
	if on, err := toggleAuto(load(), "work"); err != nil || on || load().Chats["work"].AutoSend() {
		t.Fatalf("выключение: %v %v", on, err)
	}
	if _, err := toggleAuto(load(), "news"); err == nil {
		t.Fatal("чат без отправки переключать нельзя")
	}
	if _, err := toggleAuto(load(), "gone"); err == nil {
		t.Fatal("чата нет")
	}
	if r := load().Chats["work"]; !r.Read || !r.Send {
		t.Fatalf("права чтения и отправки не трогаются: %+v", r)
	}
}
