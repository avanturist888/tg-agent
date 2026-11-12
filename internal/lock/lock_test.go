package lock

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// Файл, оставшийся от старой сборки (или не удалённый при выходе), лока не
// держит: держит только блокировка ОС.
func TestLeftoverFileIsNotLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	raw, _ := json.Marshal(meta{PID: os.Getpid(), TS: float64(time.Now().Unix())})
	if err := os.WriteFile(p+".lock", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(p, time.Second)
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("оставшийся файл принят за лок: %v", err)
	}
	l.Release()
}

// Отпустивший лок удаляет файл; тот, кто открыл прежний файл до удаления и
// запер его после, лока не получает — путь ведёт уже к другому файлу.
func TestReplacedFileNotTaken(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	a := New(p, time.Second)
	if err := a.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	stale, err := openLock(p+".lock", true) // ждущий: файл открыт, лок не взят
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Close()
	if ok, _ := tryLock(stale); ok {
		t.Fatal("взят лок, который держит a")
	}
	a.Release()
	c := New(p, 0)
	cOK, err := c.tryAcquire()
	if err != nil {
		t.Fatal(err)
	}
	ok, err := tryLock(stale)
	if err != nil {
		t.Fatal(err)
	}
	if ok && samePath(stale, p+".lock") {
		if cOK {
			t.Fatal("лок держат двое")
		}
		t.Fatal("заперт удалённый файл, а захват его принял бы")
	}
	if ok {
		unlock(stale)
	}
	if cOK {
		c.Release()
	}
}

// Лок другого процесса исключает нас, а с его смертью снимается сам.
func TestLockAcrossProcesses(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHold$")
	cmd.Env = append(os.Environ(), "LOCK_HELPER="+p)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "held" {
		t.Fatalf("помощник не взял лок: %q %v", line, err)
	}
	if err := New(p, 300*time.Millisecond).Acquire(context.Background()); !IsBusy(err) {
		t.Fatalf("лок живого процесса взят: %v", err)
	}
	if present, alive := Peek(p); !present || !alive {
		t.Fatalf("Peek живого лока: %v %v", present, alive)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	l := New(p, 5*time.Second)
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("лок умершего процесса не снялся: %v", err)
	}
	l.Release()
}

func TestHelperHold(t *testing.T) {
	p := os.Getenv("LOCK_HELPER")
	if p == "" {
		t.Skip("только помощник TestLockAcrossProcesses")
	}
	if err := New(p, time.Second).Acquire(context.Background()); err != nil {
		os.Exit(2)
	}
	os.Stdout.WriteString("held\n")
	time.Sleep(time.Minute)
	os.Exit(0)
}

// Peek не удаляет файл и не считает вечно живым файл старой сборки без
// владельца: упал между созданием и записью — по возрасту.
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

// Горутины одного процесса берут один лок по очереди.
func TestLockExclusiveInProcess(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	var inside, worst atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				l := New(p, time.Minute)
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

// Лок сборки до OS-локов — только запись в файле: живой владелец в ней
// исключает нас, мёртвый — нет. Пустой молодой файл, созданный не нами, —
// его как раз пишут; давний пустой — брошен.
func TestOldBuildOwner(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	put := func(body string, age time.Duration) {
		if err := os.WriteFile(p+".lock", []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(p+".lock", at, at); err != nil {
			t.Fatal(err)
		}
	}
	try := func() bool {
		l := New(p, 0)
		ok, err := l.tryAcquire()
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			l.Release()
		}
		return ok
	}
	now := float64(time.Now().Unix())
	put(fmt.Sprintf(`{"pid": %d, "ts": %f}`, os.Getppid(), now), 0)
	if try() {
		t.Fatal("взят лок живого процесса старой сборки")
	}
	if present, alive := Peek(p); !present || !alive {
		t.Fatal("Peek: лок старой сборки не живой")
	}
	put(`{"pid": 999999, "ts": 1}`, 0)
	if !try() {
		t.Fatal("лок умершего процесса старой сборки не взят")
	}
	put("", 0)
	if try() {
		t.Fatal("молодой пустой файл (старая сборка его пишет) принят за свободный")
	}
	put("", time.Minute)
	if !try() {
		t.Fatal("давний пустой файл не взят")
	}
	put(fmt.Sprintf(`{"pid": %d, "ts": %f, "os": true}`, os.Getppid(), now), 0)
	if !try() {
		t.Fatal("незапертый файл сборки с OS-локами принят за занятый")
	}
	put(`{"os": true}`, 0)
	if present, alive := Peek(p); !present || alive {
		t.Fatal("Peek: отпущенный лок живой")
	}
}
