// Package envfile — чтение и правка .env без потери комментариев.
package envfile

import (
	"os"
	"strings"
)

// Read — значения из файла (без учёта окружения процесса).
func Read(path string) map[string]string {
	out := map[string]string{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		key, value, ok := strings.Cut(s, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `'"`)
	}
	return out
}

// Set задаёт key=value: правит существующую строку (в том числе
// закомментированную «# KEY=»), иначе дописывает в конец.
func Set(path, key, value string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	line := key + "=" + value
	commented := -1
	for i, l := range lines {
		s := strings.TrimSpace(l)
		if k, _, ok := strings.Cut(s, "="); ok && strings.TrimSpace(k) == key {
			lines[i] = line
			return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
		}
		if strings.HasPrefix(s, "#") {
			if k, _, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(s, "#")), "="); ok && strings.TrimSpace(k) == key && commented < 0 {
				commented = i
			}
		}
	}
	if commented >= 0 {
		lines[commented] = line
	} else {
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, line, "")
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
}
