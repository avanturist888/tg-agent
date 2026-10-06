package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"tgagent/internal/config"
	"tgagent/internal/svc"
)

func TestTakeProfile(t *testing.T) {
	cases := []struct {
		in   []string
		rest []string
		name string
		set  bool
	}{
		{[]string{"login"}, []string{"login"}, "", false},
		{[]string{"--profile", "work", "login", "--phone", "+7"}, []string{"login", "--phone", "+7"}, "work", true},
		{[]string{"mcp", "--profile=work"}, []string{"mcp"}, "work", true},
		{[]string{"serve", "--worker", "--profile", "work"}, []string{"serve", "--worker"}, "work", true},
	}
	for _, c := range cases {
		rest, name, set, err := takeProfile(c.in)
		if err != nil || !reflect.DeepEqual(rest, c.rest) || name != c.name || set != c.set {
			t.Fatalf("%v → %v %q %v %v", c.in, rest, name, set, err)
		}
	}
	if _, _, _, err := takeProfile([]string{"login", "--profile"}); err == nil {
		t.Fatal("--profile без имени")
	}
}

func TestInitProfile(t *testing.T) {
	old := config.Root
	config.Root = t.TempDir()
	t.Cleanup(func() {
		config.Root = old
		_ = config.SetProfile("")
	})
	os.WriteFile(filepath.Join(config.Root, ".env"), []byte("TG_API_ID=12345\nTG_API_HASH=abc\nTG_BOT_TOKEN=main-bot\nTG_APPROVAL_CHAT_ID=1\nTG_PHONE=+7111\n"), 0o600)
	mainAddr := svc.Address()

	if err := config.SetProfile("work"); err != nil {
		t.Fatal(err)
	}
	if svc.Address() == mainAddr {
		t.Fatal("у профиля должен быть свой канал службы")
	}
	if code := cmdInit(context.Background(), []string{"--phone", "+7222"}); code != 0 {
		t.Fatalf("init: код %d", code)
	}
	env, _ := os.ReadFile(config.EnvPath())
	text := string(env)
	if !strings.Contains(text, "TG_API_ID=12345") || !strings.Contains(text, "TG_PHONE=+7222") ||
		strings.Contains(text, "main-bot") || strings.Contains(text, "+7111") || !strings.Contains(text, "TG_SEND_POLICY=bot_approval") {
		t.Fatalf(".env профиля:\n%s", text)
	}
	s, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !s.SharedBot || s.BotToken != "main-bot" || s.CallbackPrefix() != "@work|" {
		t.Fatalf("профиль без своего бота берёт бот основного: shared=%v prefix=%q", s.SharedBot, s.CallbackPrefix())
	}
	if _, ok := s.Chats["saved"]; !ok || s.Phone != "+7222" || !strings.HasPrefix(s.SessionPath, config.Home()) {
		t.Fatalf("профиль после init: чаты %v, номер %q, сессия %s", s.ChatOrder, s.Phone, s.SessionPath)
	}
	// повторный init ничего не перезаписывает
	os.WriteFile(config.EnvPath(), append(env, []byte("# правка владельца\n")...), 0o600)
	if code := cmdInit(context.Background(), nil); code != 0 {
		t.Fatalf("повторный init: код %d", code)
	}
	if again, _ := os.ReadFile(config.EnvPath()); !strings.Contains(string(again), "# правка владельца") {
		t.Fatal("init перезаписал .env профиля")
	}

	// основной профиль: канал прежний
	if err := config.SetProfile(""); err != nil {
		t.Fatal(err)
	}
	if svc.Address() != mainAddr {
		t.Fatal("канал основного профиля поменялся")
	}
}
