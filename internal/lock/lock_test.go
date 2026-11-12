package lock

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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

// Peek не снимает лок и не считает вечно живым файл без владельца: упал
// между созданием и записью — по возрасту, как breakIfStale.
func TestPeekMalformed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x")
	if present, _ := Peek(p); present {
		t.Fatal("нет файла — а Peek видит")
	}
	if err := os.WriteFile(p+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if present, alive := Peek(p); !present || !alive {
		t.Fatal("свежий пустой лок — его как раз пишут")
	}
	old := time.Now().Add(-2 * StaleAfter)
	if err := os.Chtimes(p+".lock", old, old); err != nil {
		t.Fatal(err)
	}
	if present, alive := Peek(p); !present || alive {
		t.Fatalf("старый пустой лок: present %v alive %v", present, alive)
	}
	if _, err := os.Stat(p + ".lock"); err != nil {
		t.Fatal("Peek снял лок")
	}
}

// Горутины одного процесса берут один лок по очереди: файл со своим pid,
// ещё не отмеченный в held, не принимается за осиротевший.
func TestLockExclusiveInProcess(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	var inside, worst atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				l := New(p, 10*time.Second)
				if err := l.Acquire(context.Background()); err != nil {
					t.Error(err)
					return
				}
				if n := inside.Add(1); n > worst.Load() {
					worst.Store(n)
				}
				time.Sleep(time.Millisecond)
				inside.Add(-1)
				l.Release()
			}
		}()
	}
	wg.Wait()
	if worst.Load() != 1 {
		t.Fatalf("под локом одновременно %d", worst.Load())
	}
}
