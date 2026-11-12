package core

import (
	"context"
	"testing"

	"tgagent/internal/bot"
	"tgagent/internal/config"
)

// Решение владельца (кнопка черновика) сдвигает записанное смещение только
// после разбора: умри процесс посреди, Telegram отдаст нажатие снова.
// Остальное (переключатель автоотправки) — до разбора: повтор переключил бы
// обратно.
func TestPumpOffsetOrder(t *testing.T) {
	s := watchEnv(t)
	s.BotToken = "77:x"
	seen := map[int64]int64{} // update_id → записанное смещение во время разбора
	old := updateHandler
	updateHandler = func(_ context.Context, s *config.Settings, u bot.Update) { seen[u.UpdateID] = readOffset(s) }
	t.Cleanup(func() { updateHandler = old })
	cb := func(id int64, data string) bot.Update {
		return bot.Update{UpdateID: id, CallbackQuery: &bot.CallbackQuery{ID: "q", Data: data}}
	}
	off := pumpUpdates(context.Background(), s, 10, []bot.Update{
		cb(10, "d:abc:ok"), cb(11, "s:chat~y"), cb(12, "@work|d:x:no"),
	})
	if off != 13 || readOffset(s) != 13 {
		t.Fatalf("смещение после пачки: %d, записано %d", off, readOffset(s))
	}
	if seen[10] != 10 || seen[12] != 12 {
		t.Fatalf("решение сдвинуло смещение до разбора: %v", seen)
	}
	if seen[11] != 12 {
		t.Fatalf("переключатель — смещение не сдвинуто до разбора: %v", seen)
	}
}

// Решение, разбор которого раз за разом роняет процесс, пропускается после
// maxUpdateTries начал — служба не падает по кругу.
func TestPumpPoisonSkipped(t *testing.T) {
	s := watchEnv(t)
	s.BotToken = "77:x"
	handled := 0
	old := updateHandler
	updateHandler = func(context.Context, *config.Settings, bot.Update) { handled++ }
	t.Cleanup(func() { updateHandler = old })
	u := bot.Update{UpdateID: 5, CallbackQuery: &bot.CallbackQuery{ID: "q", Data: "d:abc:ok"}}
	// процесс «умирал» посреди разбора: начат, смещение не сдвинуто
	for range maxUpdateTries {
		startInflight(s, 5, 5)
	}
	if off := pumpUpdates(context.Background(), s, readOffset(s), []bot.Update{u}); off != 6 || handled != 0 {
		t.Fatalf("смещение %d, разобрано %d", off, handled)
	}
	if readOffset(s) != 6 {
		t.Fatalf("пропущенное не сдвинуло смещение: %d", readOffset(s))
	}
}
