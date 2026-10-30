//go:build live

package core

import (
	"context"
	"testing"

	"tgagent/internal/config"
)

// TestLiveWakeAgent — путь, которым служба отдаёт карточку обновления агенту:
// Нестор будит агента проекта в новом треде. Тред настоящий — текст просит
// его ничего не делать.
// go test -tags live -run LiveWakeAgent -v ./internal/core
func TestLiveWakeAgent(t *testing.T) {
	if config.Root == "" {
		t.Skip("нет установки")
	}
	err := wakeProjectAgent(context.Background(), nil, "Проверка: пробуждение агента службой tg-agent",
		"Это живая проверка пути, которым служба tg-agent отдаёт агенту карточки обновлений MAX "+
			"(core.wakeProjectAgent → nestor send). Ничего не делай и ничего не меняй: ответь одной строкой "+
			"«проверка пробуждения получена» и закончи ход.")
	if err != nil {
		t.Fatal(err)
	}
}
