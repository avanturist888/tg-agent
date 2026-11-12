package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tgagent/internal/procs"
	"tgagent/internal/store"
)

type fakeProcs struct {
	alive      map[int]procs.Proc
	stopped    []int
	exitsAfter map[int]int // pid → сколько опросов до выхода самому
	polls      int
}

func (f *fakeProcs) list(string) ([]procs.Proc, error) {
	f.polls++
	var out []procs.Proc
	for pid, p := range f.alive {
		if n, ok := f.exitsAfter[pid]; ok && f.polls > n {
			delete(f.alive, pid)
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeProcs) terminate(p procs.Proc) error {
	f.stopped = append(f.stopped, p.PID)
	delete(f.alive, p.PID)
	return nil
}

func proc(pid int, args ...string) procs.Proc {
	return procs.Proc{PID: pid, Exe: `C:\tg\bin\tg.old-1.exe`, Args: append([]string{"tg.exe"}, args...)}
}

func legacyDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "upstream_cards.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Прослойка агента остаётся, окно и служба завершаются сразу, вызов
// инструмента дожидаются; потом — отметка.
func TestCutoverWaitsAndMarks(t *testing.T) {
	dir := legacyDir(t)
	// после tg rollback: обновление снимает отметку отката
	if err := os.WriteFile(filepath.Join(dir, store.RollbackFile), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeProcs{alive: map[int]procs.Proc{
		1: proc(1, "mcp"),
		2: proc(2, "gui"),
		3: proc(3, "--profile", "work", "serve", "--worker"),
		4: proc(4, "tool-call", "tg_read_chat"),
	}, exitsAfter: map[int]int{4: 2}}
	var out bytes.Buffer
	c := cutover{dirs: []string{dir}, wait: time.Minute, out: &out, list: f.list, terminate: f.terminate,
		sleep: func(context.Context, time.Duration) {}}
	if err := c.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.stopped) != 2 || f.alive[1].PID != 1 {
		t.Fatalf("завершены %v, живы %v", f.stopped, f.alive)
	}
	for _, pid := range f.stopped {
		if pid != 2 && pid != 3 {
			t.Fatalf("завершён %d", pid)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, store.CutoverFile)); err != nil {
		t.Fatal("нет отметки")
	}
	if store.RolledBack(dir) {
		t.Fatal("отметка отката не снята")
	}
	if !strings.Contains(out.String(), "Жду, пока закончит: 4") || !strings.Contains(out.String(), "окно управления закрыто") {
		t.Fatalf("вывод: %s", out.String())
	}
}

// Не закончил за отведённое время — завершается.
func TestCutoverStopsAfterDeadline(t *testing.T) {
	dir := legacyDir(t)
	f := &fakeProcs{alive: map[int]procs.Proc{4: proc(4, "send-due")}}
	c := cutover{dirs: []string{dir}, wait: 0, out: &bytes.Buffer{}, list: f.list, terminate: f.terminate,
		sleep: func(context.Context, time.Duration) {}}
	if err := c.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.stopped) != 1 || f.stopped[0] != 4 {
		t.Fatalf("завершены %v", f.stopped)
	}
}

// Процесс, про который не понять, что он делает, не завершаем и отметку не
// ставим: перенос подождёт.
func TestCutoverUnknownBlocks(t *testing.T) {
	dir := legacyDir(t)
	p := proc(5)
	p.Args = nil
	f := &fakeProcs{alive: map[int]procs.Proc{5: p}}
	c := cutover{dirs: []string{dir}, wait: 0, out: &bytes.Buffer{}, list: f.list, terminate: f.terminate,
		sleep: func(context.Context, time.Duration) {}}
	if err := c.run(context.Background()); err == nil {
		t.Fatal("нет ошибки")
	}
	if len(f.stopped) != 0 {
		t.Fatalf("завершены %v", f.stopped)
	}
	if _, err := os.Stat(filepath.Join(dir, store.CutoverFile)); err == nil {
		t.Fatal("отметка поставлена")
	}
}

// Старых файлов нет — процессы не трогаются (обычная сборка).
func TestCutoverNothingToDo(t *testing.T) {
	f := &fakeProcs{alive: map[int]procs.Proc{2: proc(2, "gui")}}
	c := cutover{dirs: []string{t.TempDir()}, out: &bytes.Buffer{}, list: f.list, terminate: f.terminate}
	if err := c.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.stopped) != 0 || f.polls != 0 {
		t.Fatalf("тронуты процессы: %v", f.stopped)
	}
}

// Выгрузке для отката мешает любой процесс установки, кроме прослоек агентов
// и самого tg rollback.
func TestRollbackBlockers(t *testing.T) {
	got := rollbackBlockers([]procs.Proc{proc(1, "mcp"), proc(2, "rollback"), proc(3, "serve"), proc(4, "tool-call", "tg_read_chat")}, 2)
	if len(got) != 2 || !strings.Contains(got[0], "3") || !strings.Contains(got[1], "4") {
		t.Fatalf("мешают: %q", got)
	}
}
