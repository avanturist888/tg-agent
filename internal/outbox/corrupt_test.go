package outbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// После сбоя питания outbox.json оказался целиком из нулей (9.11.2026), и
// отправка стояла у всех: испорченный файл должен уходить в сторону сам.
func TestZeroedOutboxIsQuarantined(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "outbox.json")
	os.WriteFile(path, make([]byte, 4096), 0o600)
	o := New(dir)
	d, err := o.Create(CreateOpts{Chat: "c", Text: "hi", TTLMin: 60})
	if err != nil {
		t.Fatalf("отправка встала: %v", err)
	}
	if got, err := o.Get(d.ID); err != nil || got.Text != "hi" {
		t.Fatalf("черновик не записан: %v %v", got, err)
	}
	broken, _ := filepath.Glob(path + ".broken-*")
	if len(broken) != 1 {
		t.Fatalf("испорченный файл не отложен: %v", broken)
	}
	if b, _ := os.ReadFile(broken[0]); len(b) != 4096 {
		t.Fatal("отложенный файл не тот")
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl")); !strings.Contains(string(raw), "legacy_import_failed") {
		t.Fatalf("нет записи в аудите: %s", raw)
	}
}
