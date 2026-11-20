package core

import (
	"testing"
	"time"

	"tgagent/internal/outbox"
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
