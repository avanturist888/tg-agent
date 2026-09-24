package lock

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockBasics(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	a := New(p, time.Second)
	if err := a.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := New(p, 300*time.Millisecond)
	if err := b.Acquire(context.Background()); !IsBusy(err) {
		t.Fatalf("второй захват: %v", err)
	}
	a.Release()
	if _, err := os.Stat(p + ".lock"); !os.IsNotExist(err) {
		t.Fatal("файл лока остался")
	}
}

func TestOrphanAndDeadOwner(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	// свой pid, но лок мы не держим — осиротевший
	raw, _ := json.Marshal(meta{PID: os.Getpid(), TS: 1})
	os.WriteFile(p+".lock", raw, 0o600)
	if err := New(p, time.Second).Acquire(context.Background()); err != nil {
		t.Fatalf("свой осиротевший лок не снят: %v", err)
	}
	New(p, 0).Release()
	os.Remove(p + ".lock")
	// pid живого процесса, запущенного позже взятия лока, — чужой, лок протух
	raw, _ = json.Marshal(meta{PID: os.Getppid(), TS: 1})
	os.WriteFile(p+".lock", raw, 0o600)
	if err := New(p, time.Second).Acquire(context.Background()); err != nil {
		t.Fatalf("лок с переиспользованным pid не снят: %v", err)
	}
}
