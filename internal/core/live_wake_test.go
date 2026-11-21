//go:build live

package core

import (
	"context"
	"testing"

	"tgagent/internal/config"
)

// TestLiveWakeAgent — путь, которым служба отдаёт карточку обновления агенту:
// прямая отправка в назначенный существующий тред проекта через T3 Code.
// go test -tags live -run LiveWakeAgent -v ./internal/core
func TestLiveWakeAgent(t *testing.T) {
	if config.Root == "" {
		t.Skip("нет установки")
	}
	err := wakeProjectAgent(context.Background(), nil, "Проверка: пробуждение агента службой tg-agent",
		"Это живая проверка пути, которым служба tg-agent отдаёт агенту карточки обновлений MAX "+
			"(core.wakeProjectAgent → t3.SendThread). Это служебная тестовая отметка, не поручение владельца. "+
			"Отметь получение и продолжай текущую задачу.")
	if err != nil {
		t.Fatal(err)
	}
}
