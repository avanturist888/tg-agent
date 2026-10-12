//go:build live

package stt

// Живой тест расшифровщика: go test -tags live ./internal/stt
// (нужны bin\tgstt.exe — scripts\build.ps1, модели — scripts\install-stt.ps1 — и ffmpeg).

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLiveSilence(t *testing.T) {
	if !Enabled() {
		t.Skip("расшифровщик не установлен")
	}
	path := filepath.Join(t.TempDir(), "silence.oga")
	if out, err := exec.Command("ffmpeg", "-loglevel", "error", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=mono",
		"-t", "3", "-c:a", "libopus", path).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	r, err := Transcribe(ctx, path, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Done() || r.Speech || r.Text != "" || r.Label() == "" {
		t.Fatalf("тишина: %+v", r)
	}
	// второй запрос — тот же процесс, модели уже в памяти
	start := time.Now()
	if _, err := Transcribe(ctx, path, "ru"); err != nil {
		t.Fatal(err)
	}
	t.Logf("повторный запрос: %s", time.Since(start))
	w.mu.Lock()
	w.stop()
	w.mu.Unlock()
}
