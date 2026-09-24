package outbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// Реальный outbox.json (если есть) переживает чтение и запись без потерь.
func TestRoundTripRealFile(t *testing.T) {
	src := filepath.Join("..", "..", "..", "data", "outbox.json")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Skip("нет data/outbox.json")
	}
	// только в памяти: приватные данные никуда не копируем
	var drafts []*Draft
	if err := json.Unmarshal(data, &drafts); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(drafts)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	json.Unmarshal(data, &a)
	json.Unmarshal(out, &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("после перезаписи содержимое отличается")
	}
}

func TestExtraFieldsKeptAndScheduled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "outbox.json")
	os.WriteFile(path, []byte(`[{"future_field": {"x": 1}, "id": "a", "chat": "c", "text": "t", "reply_to": null,
	  "created_at": "2026-01-01T00:00:00+00:00", "expires_at": "2099-01-01T00:00:00+00:00", "status": "pending",
	  "files": [], "history": []}]`), 0o600)
	o := New(path)
	if _, err := o.Approve("a", "x"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var v []map[string]any
	json.Unmarshal(raw, &v)
	if v[0]["future_field"] == nil || v[0]["status"] != "approved" {
		t.Fatalf("потеряно поле или статус: %s", raw)
	}
	at := time.Now().Add(-time.Second)
	d, err := o.Create(CreateOpts{Chat: "c", Text: "hi", TTLMin: 60, SendAt: &at})
	if err != nil || d.Status != Scheduled {
		t.Fatal(err, d.Status)
	}
	due, err := o.ClaimDue(time.Now())
	if err != nil || len(due) != 1 || due[0].ID != d.ID || due[0].ApprovedBy() != "auto_send" {
		t.Fatal("ClaimDue", err, len(due))
	}
	due, _ = o.ClaimDue(time.Now())
	if len(due) != 0 {
		t.Fatal("повторный захват")
	}
}
