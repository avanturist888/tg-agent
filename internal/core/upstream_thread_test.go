package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"tgagent/internal/config"
)

func TestUpstreamWakeThread(t *testing.T) {
	old, oldSend := config.Root, nestorSend
	config.Root = t.TempDir()
	t.Setenv("TG_NESTOR", "nestor-test")
	t.Cleanup(func() { config.Root, nestorSend = old, oldSend })
	var calls [][]string
	fail := map[string]bool{}
	nestorSend = func(_ context.Context, exe, text string, args ...string) error {
		calls = append(calls, append([]string{text}, args...))
		if fail[args[1]] {
			return errors.New("тред закрыт")
		}
		return nil
	}
	s := &config.Settings{}

	// без назначенного — новый тред, как было
	if err := wakeProjectAgent(context.Background(), s, "Заголовок", "задание"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls[0][1:], " "); got != "send "+config.Root+" - --new --title Заголовок" {
		t.Fatalf("новый тред: %q", got)
	}

	// назначенный — туда, с заголовком в тексте
	if err := SetUpstreamThread("mcp:abc"); err != nil {
		t.Fatal(err)
	}
	calls = nil
	if err := wakeProjectAgent(context.Background(), s, "Заголовок", "задание"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || strings.Join(calls[0][1:], " ") != "send mcp:abc -" || !strings.HasPrefix(calls[0][0], "«Заголовок»") {
		t.Fatalf("назначенный тред: %q", calls)
	}

	// не дошло — новый тред
	fail["mcp:abc"] = true
	calls = nil
	if err := wakeProjectAgent(context.Background(), s, "Заголовок", "задание"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[1][4] != "--new" {
		t.Fatalf("запасной путь: %q", calls)
	}

	if err := SetUpstreamThread(""); err != nil || ReadUpstreamThread() != nil {
		t.Fatalf("сброс: %v", err)
	}
}
