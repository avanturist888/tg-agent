package t3

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWakeWithoutT3(t *testing.T) {
	t.Setenv("T3CODE_HOME", filepath.Join(t.TempDir(), "нет"))
	start := time.Now()
	err := Wake(context.Background(), filepath.Join(t.TempDir(), "tok.json"), "s", "x")
	if !errors.Is(err, Unavailable) {
		t.Fatalf("без T3 Code ждали Unavailable, получили %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("без T3 Code ответ должен быть мгновенным")
	}
}

func TestWakeT3NotRunning(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("T3CODE_HOME", dir)
	os.MkdirAll(filepath.Join(dir, "userdata"), 0o700)
	// T3 был запущен, но закрыт: порт никто не слушает
	os.WriteFile(filepath.Join(dir, "userdata", "server-runtime.json"), []byte(`{"port":1}`), 0o600)
	if err := Wake(context.Background(), filepath.Join(dir, "tok.json"), "s", "x"); !errors.Is(err, Unavailable) {
		t.Fatalf("незапущенный T3 Code: ждали Unavailable, получили %v", err)
	}
}

func TestTokenSession(t *testing.T) {
	if got := tokenSession("eyJzaWQiOiJhYmMifQ.sig"); got != "abc" {
		t.Fatalf("sid: %q", got)
	}
	if tokenSession("мусор") != "" {
		t.Fatal("из мусора sid не берётся")
	}
}
