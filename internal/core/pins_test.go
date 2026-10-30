package core

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"tgagent/internal/config"
)

func TestDraftCardPins(t *testing.T) {
	old := config.Root
	config.Root = t.TempDir()
	os.MkdirAll(config.DataPath(), 0o700)
	var calls []string
	oldBot := pinBot
	pinBot = func(_ context.Context, _ *config.Settings, method string, payload any, _ any) error {
		calls = append(calls, fmt.Sprintf("%s:%v", method, payload.(map[string]any)["message_id"]))
		return nil
	}
	t.Cleanup(func() { config.Root, pinBot = old, oldBot })
	s := &config.Settings{BotToken: "x", ApprovalChatID: 42}
	ctx := context.Background()

	pinDraftCard(ctx, s, "d1", 7)
	pinDraftCard(ctx, s, "d2", 8)
	unpinDraftCard(ctx, s, "d1")
	unpinDraftCard(ctx, s, "d1") // уже снята — Telegram не трогаем
	// черновика d2 в очереди нет — уборка снимает его карточку
	sweepPins(ctx, s)
	if got := strings.Join(calls, "|"); got != "pinChatMessage:7|pinChatMessage:8|unpinChatMessage:7|unpinChatMessage:8" {
		t.Fatalf("вызовы: %s", got)
	}
	_ = editPins(s, func(m map[string]int64) {
		if len(m) != 0 {
			t.Fatalf("остались закреплённые: %v", m)
		}
	})
}
