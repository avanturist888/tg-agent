//go:build live

package t3

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"tgagent/internal/config"
)

// go test -tags live -run Live ./internal/t3 — из треда T3 Code: в этот тред
// придёт сообщение от tg-agent.
func TestLiveWakeOwnThread(t *testing.T) {
	session := os.Getenv("CLAUDE_CODE_SESSION_ID")
	if session == "" {
		t.Skip("запущено не из сессии Claude Code")
	}
	in, err := find(context.Background())
	if err != nil {
		t.Skip(err)
	}
	thread, err := in.threadFor(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("тред %s", thread)
	path := filepath.Join(config.Root, "data", "t3-token.json")
	if err := Wake(context.Background(), path, session, "tg-agent: проверка пробуждения через T3 Code — ответь одним словом «принято»."); err != nil {
		t.Fatal(err)
	}
}
