package core

import (
	"os"
	"testing"

	"tgagent/internal/config"
)

func TestOffsetFollowsBot(t *testing.T) {
	old := config.Root
	config.Root = t.TempDir()
	t.Cleanup(func() { config.Root = old })
	os.MkdirAll(config.DataPath(), 0o700)
	first := &config.Settings{BotToken: "111:aaa"}
	second := &config.Settings{BotToken: "222:bbb"}

	// старая запись без id бота: служба при старте подписывает её текущим ботом
	os.WriteFile(first.OffsetPath(), []byte(`{"offset": 900}`), 0o600)
	tagOffset(first)
	if got := readOffset(first); got != 900 {
		t.Fatalf("тот же бот: смещение %d", got)
	}
	// бот сменили — его апдейты с начала, иначе нажатия съедятся
	if got := readOffset(second); got != 0 {
		t.Fatalf("новый бот: смещение %d, ждали 0", got)
	}
	writeOffset(second, 5)
	if readOffset(second) != 5 || readOffset(first) != 0 {
		t.Fatal("смещение принадлежит последнему боту")
	}
}
