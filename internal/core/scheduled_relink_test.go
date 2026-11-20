package core

import (
	"context"
	"testing"
	"time"

	"tgagent/internal/outbox"
	"tgagent/internal/store"
)

// Отметка sent-unsaved с id очереди переписывается на реальный: иначе
// finishUnsaved вернула бы id очереди в уже перезаписанный черновик.
func TestRelinkUnsavedFile(t *testing.T) {
	s := watchEnv(t)
	box := outbox.New(s.DataDir())
	d, err := box.Create(outbox.CreateOpts{Chat: "saved", Text: "файл", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Approve(d.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := box.MarkSent(d.ID, 1000, time.Now().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if err := writeUnsaved(s, d.ID, 1000, "2026-11-20T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	relinked, err := box.RelinkScheduled([][2]int64{{1000, 3000}})
	if err != nil {
		t.Fatal(err)
	}
	relinkUnsaved(s, relinked)
	if mid, ok := unsavedSent(s, d.ID); !ok || mid != 3000 {
		t.Fatalf("в файле message_id %d (есть: %v), ждали 3000", mid, ok)
	}
	got, err := box.Get(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MessageID == nil || *got.MessageID != 3000 {
		t.Fatalf("message_id = %v, ждали 3000", got.MessageID)
	}
}

// Гонка с большим файлом: доставка случилась, пока черновик ещё не был
// помечен отправленным (загрузка шла между ответом Telegram и MarkSent) —
// событие его не нашло, но пара осталась в базе, и уборка до-связала.
func TestScheduledRelinkAfterMarkSent(t *testing.T) {
	s := watchEnv(t)
	box := outbox.New(s.DataDir())
	d, err := box.Create(outbox.CreateOpts{Chat: "saved", Text: "текст и файл", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Approve(d.ID, "test"); err != nil {
		t.Fatal(err)
	}
	// доставка: событие пришло, черновик ещё approved без message_id —
	// связывать нечего, пара запоминается в базе
	rememberScheduledLinks(s, [][2]int64{{94, 3614944}})
	// отправитель доджлал загрузку и записал id очереди
	if _, err := box.MarkSent(d.ID, 94, time.Now().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if got, err := box.Get(d.ID); err != nil || got.MessageID == nil || *got.MessageID != 94 {
		t.Fatalf("до уборки: %v %v", got.MessageID, err)
	}
	// уборка службы — раз в минуту
	relinkScheduledLinks(context.Background(), s)
	got, err := box.Get(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MessageID == nil || *got.MessageID != 3614944 {
		t.Fatalf("message_id = %v, ждали 3614944", got.MessageID)
	}
	hist := got.HistoryEvents()
	last := hist[len(hist)-1]
	if last["event"] != "scheduled_sent" || last["queue_id"] != float64(94) {
		t.Fatalf("история: %v", last)
	}
	// повторная уборка не меняет ничего
	relinkScheduledLinks(context.Background(), s)
	if got, err := box.Get(d.ID); err != nil || got.MessageID == nil || *got.MessageID != 3614944 {
		t.Fatalf("после повтора: %v %v", got.MessageID, err)
	}
	// протухшие пары выкидываются
	db, err := store.Open(s.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	var links []scheduledLink
	if ok, _ := store.GetKV(context.Background(), db.Q(), store.KeyScheduledLinks, &links); !ok || len(links) != 1 || links[0].Queue != 94 {
		t.Fatalf("пары в базе: %v %v", links, ok)
	}
}
