package mcpproxy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPiSlug(t *testing.T) {
	for dir, want := range map[string]string{
		`C:\Users\vasis\work\maria-shora`:          "--C--Users-vasis-work-maria-shora--",
		`C:\Users\vasis\tg-agent`:                  "--C--Users-vasis-tg-agent--",
		`C:\Users\vasis\projects_hobby\mama-nalog`: "--C--Users-vasis-projects_hobby-mama-nalog--",
	} {
		if got := piSlug(dir); got != want {
			t.Fatalf("piSlug(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestPiSessionAddrInPicksFreshNewest(t *testing.T) {
	home := t.TempDir()
	cwd := `C:\Users\vasis\work\maria-shora`
	dir := filepath.Join(home, piSlug(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	write := func(name string, age time.Duration) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	write("2026-11-16T19-28-26Z_old.jsonl", 20*time.Minute)
	write("2026-11-17T09-00-00Z_current.jsonl", time.Minute)
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nested.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}

	a := piSessionAddrIn(home, cwd, now)
	if a == nil {
		t.Fatal("fresh session file not found")
	}
	if !a.Codex() || filepath.Base(a.Session) != "2026-11-17T09-00-00Z_current.jsonl" {
		t.Fatalf("wrong pick: %+v", a)
	}
	if a.Session != filepath.Join(dir, "2026-11-17T09-00-00Z_current.jsonl") {
		t.Fatalf("full path wrong: %q", a.Session)
	}
}

func TestPiSessionAddrInRefusesStaleAndMissing(t *testing.T) {
	home := t.TempDir()
	cwd := `C:\Users\vasis\work\maria-shora`
	dir := filepath.Join(home, piSlug(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	p := filepath.Join(dir, "old.jsonl")
	if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Один файл в каталоге однозначен, даже если он не растёт (сессия простаивает).
	if a := piSessionAddrIn(home, cwd, now); a == nil || filepath.Base(a.Session) != "old.jsonl" {
		t.Fatalf("single stale session not picked: %+v", a)
	}
	// Второй файл делает выбор неоднозначным: без свежести не угадываем.
	write := filepath.Join(dir, "younger.jsonl")
	if err := os.WriteFile(write, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(write, now.Add(-30*time.Minute), now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if a := piSessionAddrIn(home, cwd, now); a != nil {
		t.Fatalf("ambiguous stale sessions accepted: %+v", a)
	}
	if a := piSessionAddrIn(home, `C:\no\such\workspace`, now); a != nil {
		t.Fatalf("missing dir accepted: %+v", a)
	}
	if a := piSessionAddrIn("", cwd, now); a != nil {
		t.Fatalf("empty home accepted: %+v", a)
	}
}
