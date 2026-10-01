//go:build live

package inbox

import (
	"context"
	"testing"
)

// go test -tags live -run Live ./internal/inbox — из сессии Claude Code:
// в неё придёт сообщение от tg-agent.
func TestLiveSendToOwnSession(t *testing.T) {
	a := FromEnv()
	if a == nil {
		t.Skip("запущено не из сессии Claude Code")
	}
	if err := Send(context.Background(), *a, "tg-agent", "Проверка канала tg-agent: ответь одним словом «принято»."); err != nil {
		t.Fatal(err)
	}
}
