// Package audit — журнал data/audit.jsonl: что агенты делали с аккаунтом.
package audit

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"tgagent/internal/omap"
)

var mu sync.Mutex

// Now — текущее время UTC с точностью до секунды (2026-09-24T10:35:02+00:00).
func Now() string { return FormatTime(time.Now()) }

func FormatTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05-07:00") }

// Log дописывает строку в журнал: ts, pid, action и поля в переданном порядке
// (пары ключ, значение). Журнал — единственный источник правды о том, что
// агенты делали с аккаунтом, поэтому ошибки записи не глотаем молча.
func Log(path, action string, kv ...any) error {
	rec := omap.New().Set("ts", Now()).Set("pid", os.Getpid()).Set("action", action)
	for i := 0; i+1 < len(kv); i += 2 {
		key, _ := kv[i].(string)
		rec.Set(key, kv[i+1])
	}
	line, err := omap.Marshal(rec)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}
