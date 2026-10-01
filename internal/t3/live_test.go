//go:build live

package t3

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// go test -tags live -run LiveEnv ./internal/t3 — окружение-контейнер:
// T3_LIVE_ORIGIN (по умолчанию http://127.0.0.1:3774), токен — файлом
// T3_LIVE_TOKEN_FILE или выпуском через T3_LIVE_CONTAINER (docker exec).
// Создаёт тред «TG: проверка tg-agent» и ждёт короткий ход.
func TestLiveEnvTurn(t *testing.T) {
	origin := os.Getenv("T3_LIVE_ORIGIN")
	if origin == "" {
		origin = "http://127.0.0.1:3774"
	}
	e := &Env{Name: "live", Origin: origin, TokenPath: filepath.Join(t.TempDir(), "t3-token-live.json"),
		TokenFile: os.Getenv("T3_LIVE_TOKEN_FILE"), Model: os.Getenv("T3_LIVE_MODEL"),
		TurnTimeout: 3 * time.Minute}
	if c := os.Getenv("T3_LIVE_CONTAINER"); c != "" {
		e.PairingCmd = []string{"docker", "exec", c, "t3", "auth", "pairing", "create", "--ttl", "5m", "--label", "tg-agent", "--json"}
	}
	if e.Model == "" {
		e.Model = "claude-sonnet-5-5"
	}
	ctx := context.Background()
	label, version, err := e.Descriptor(ctx)
	if err != nil {
		t.Skip(err)
	}
	t.Logf("окружение %s, T3 %s", label, version)
	id, err := e.CreateThread(ctx, "TG: проверка tg-agent")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("тред %s", id)
	start := time.Now()
	prev, err := e.StartTurn(ctx, id, "Ответь одним словом: готов")
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.WaitTurn(ctx, id, prev)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ход %s: %s за %s, ответ %q", res.TurnID, res.State, time.Since(start).Round(time.Second), res.Text)
	if res.State != "completed" || res.Text == "" {
		t.Fatalf("ход не завершился как надо: %+v", res)
	}
	// второй ход в том же треде: прошлый не должен сойти за новый
	prev2, err := e.StartTurn(ctx, id, "Ещё раз, одним словом: готов")
	if err != nil {
		t.Fatal(err)
	}
	if prev2 != res.TurnID {
		t.Fatalf("прошлый ход %q, а не %q", prev2, res.TurnID)
	}
	res2, err := e.WaitTurn(ctx, id, prev2)
	if err != nil || res2.TurnID == res.TurnID || res2.State != "completed" {
		t.Fatalf("второй ход: %+v %v", res2, err)
	}
	t.Logf("второй ход %s: %s, ответ %q", res2.TurnID, res2.State, res2.Text)
}
