// Package attach — файлы, которые агент хочет отправить.
//
// При создании черновика файл копируется в data/outbox_files/<draft_id>/:
// человек одобряет именно этот снимок, и именно он уходит. Иначе агент мог бы
// подменить содержимое между нажатием кнопки и отправкой.
package attach

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"tgagent/internal/config"
	"tgagent/internal/outbox"
)

// Секреты не уезжают в Telegram даже с одобрения: в карточке их легко не заметить.
var denyNames = []string{
	".env", ".env.*", "*.session", "*.session-journal", "id_rsa*", "id_ed25519*", "id_ecdsa*",
	"*.pem", "*.key", "*.p12", "*.pfx", "*.kdbx", "credentials*", ".netrc", ".git-credentials",
	"*.session.json",
}

var denyDirs = map[string]bool{".ssh": true, ".gnupg": true, ".aws": true, ".azure": true, ".kube": true, ".docker": true}

// Refused — файл нельзя отправлять (секрет, файл самого tg-agent).
type Refused struct{ Msg string }

func (e *Refused) Error() string { return e.Msg }

// Bad — ошибка во входных данных (нет файла, пустой, слишком большой).
type Bad struct{ Msg string }

func (e *Bad) Error() string { return e.Msg }

func isWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

func deniedReason(path string) string {
	if isWithin(strings.ToLower(path), strings.ToLower(config.Root)) {
		return "файлы самого tg-agent (сессия, .env, очередь) не отправляются"
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if denyDirs[strings.ToLower(part)] {
			return "файлы из каталогов с ключами и учётками не отправляются"
		}
	}
	name := strings.ToLower(filepath.Base(path))
	for _, pattern := range denyNames {
		if ok, _ := filepath.Match(pattern, name); ok {
			return "похоже на секрет (ключ, сессия, .env) — такое не отправляется"
		}
	}
	return ""
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// Snapshot проверяет файлы и снимает с них копии для черновика.
func Snapshot(s *config.Settings, draftID string, paths []string) ([]outbox.FileSnap, error) {
	if len(paths) > 10 {
		return nil, &Bad{"Не больше 10 файлов в одном сообщении (так группирует Telegram)."}
	}
	limit := int64(s.MaxFileMB) * 1024 * 1024
	type checked struct {
		path string
		size int64
	}
	var ok []checked
	for _, raw := range paths {
		abs, err := filepath.Abs(expandHome(raw))
		if err == nil {
			if real, err := filepath.EvalSymlinks(abs); err == nil {
				abs = real
			}
		}
		st, err := os.Stat(abs)
		if err != nil || !st.Mode().IsRegular() {
			return nil, &Bad{"Файл не найден: " + raw}
		}
		if reason := deniedReason(abs); reason != "" {
			return nil, &Refused{fmt.Sprintf("%s: %s.", filepath.Base(abs), reason)}
		}
		if st.Size() == 0 {
			return nil, &Bad{"Файл пустой: " + filepath.Base(abs)}
		}
		if st.Size() > limit {
			return nil, &Bad{fmt.Sprintf("%s: %d МБ, предел %d МБ.", filepath.Base(abs), st.Size()/(1024*1024), s.MaxFileMB)}
		}
		ok = append(ok, checked{abs, st.Size()})
	}

	target := filepath.Join(s.OutboxFilesDir(), draftID)
	var result []outbox.FileSnap
	for i, c := range ok {
		// своя подпапка на файл: имя остаётся исходным — Telegram берёт его
		// из пути, а одноимённые файлы из разных мест не затрут друг друга
		slot := filepath.Join(target, strconv.Itoa(i))
		if err := os.MkdirAll(slot, 0o700); err != nil {
			return nil, err
		}
		copyPath := filepath.Join(slot, filepath.Base(c.path))
		if err := copyFile(c.path, copyPath); err != nil {
			return nil, err
		}
		result = append(result, outbox.FileSnap{Path: copyPath, Name: filepath.Base(c.path), Size: c.size, Source: c.path})
	}
	return result, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if st, err := os.Stat(src); err == nil {
		_ = os.Chtimes(dst, st.ModTime(), st.ModTime())
	}
	return nil
}

// Drop убирает снимки, когда черновик отправлен или отменён.
func Drop(s *config.Settings, draftID string) {
	_ = os.RemoveAll(filepath.Join(s.OutboxFilesDir(), draftID))
}

// HumanSize — 16 КБ, 3.2 ГБ и т.п.
func HumanSize(size int64) string {
	v := float64(size)
	for _, unit := range []string{"Б", "КБ", "МБ"} {
		if v < 1024 {
			return fmt.Sprintf("%.0f %s", v, unit)
		}
		v /= 1024
	}
	return fmt.Sprintf("%.1f ГБ", v)
}
