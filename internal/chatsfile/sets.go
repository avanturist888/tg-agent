package chatsfile

// Правка правил на группы чатов ([[folder]], [public_channels]) и [manage].

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"tgagent/internal/config"
)

// Folder — правило на папку.
type Folder struct {
	ID   string // id из tg_list_folders ("" — только по названию)
	Name string
	Read bool
	Send bool
	Auto bool
	Note string
}

// table — границы таблицы header ("[[folder]]", "[manage]"), у которой
// match(ключ, значение) истинно хоть для одной строки (nil — первая такая).
func table(lines []string, header string, match func(key, value string) bool) (int, int) {
	var headers []int
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			headers = append(headers, i)
		}
	}
	for n, h := range headers {
		if strings.TrimSpace(stripComment(lines[h])) != header {
			continue
		}
		end := len(lines)
		if n+1 < len(headers) {
			end = headers[n+1]
		}
		if match == nil {
			return h, end
		}
		for i := h + 1; i < end; i++ {
			key, value, ok := strings.Cut(strings.TrimSpace(lines[i]), "=")
			if ok && match(strings.TrimSpace(key), unquoteTOML(value)) {
				return h, end
			}
		}
	}
	return -1, -1
}

func folderMatch(id, name string) func(key, value string) bool {
	return func(key, value string) bool {
		if id != "" {
			return key == "id" && value == id
		}
		return key == "name" && strings.EqualFold(value, strings.TrimSpace(name))
	}
}

func edit(fn func(lines []string) ([]string, error)) error {
	path := config.ChatsFile()
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := strings.ReplaceAll(string(old), "\r\n", "\n")
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	lines, err := fn(strings.SplitAfter(text, "\n"))
	if err != nil {
		return err
	}
	return save(path, lines, old)
}

// setKeys — задать ключи таблицы по порядку (start..end заново ищет find).
func setKeys(lines []string, find func([]string) (int, int), kv [][2]string) []string {
	for _, p := range kv {
		start, end := find(lines)
		if p[1] == "" {
			lines = removeKey(lines, start, end, p[0])
		} else {
			lines = setKey(lines, start, end, p[0], p[1])
		}
	}
	return lines
}

func appendTable(lines []string, header string) []string {
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return append(lines, "\n", header+"\n")
}

// SetFolder — открыть папку (или поменять права на неё). Ищется по id, без
// id — по названию.
func SetFolder(f Folder) error {
	return edit(func(lines []string) ([]string, error) {
		find := func(l []string) (int, int) { return table(l, "[[folder]]", folderMatch(f.ID, f.Name)) }
		if start, _ := find(lines); start < 0 && f.ID != "" && f.Name != "" {
			// папка была записана названием — допишем ей id
			find = func(l []string) (int, int) { return table(l, "[[folder]]", folderMatch("", f.Name)) }
		}
		if start, _ := find(lines); start < 0 {
			// новая таблица — последняя в файле: её строки всегда до конца
			lines = appendTable(lines, "[[folder]]")
			last := len(lines) - 1
			find = func(l []string) (int, int) { return last, len(l) }
		}
		auto := ""
		if f.Send && f.Auto {
			auto = "true"
		}
		note := ""
		if f.Note != "" {
			note = quote(f.Note)
		}
		name := ""
		if f.Name != "" {
			name = quote(f.Name)
		}
		return setKeys(lines, find, [][2]string{{"id", idValue(f.ID)}, {"name", name},
			{"read", boolStr(f.Read)}, {"send", boolStr(f.Send)}, {"auto_send", auto}, {"note", note}}), nil
	})
}

func idValue(id string) string {
	if id == "" {
		return ""
	}
	if _, err := strconv.ParseInt(id, 10, 64); err == nil {
		return id
	}
	return quote(id)
}

// RemoveFolder — закрыть папку.
func RemoveFolder(id, name string) error {
	return edit(func(lines []string) ([]string, error) {
		start, end := table(lines, "[[folder]]", folderMatch(id, name))
		if start < 0 && id != "" && name != "" {
			start, end = table(lines, "[[folder]]", folderMatch("", name))
		}
		if start < 0 {
			return nil, fmt.Errorf("папки %s нет в config/chats.toml", folderRef(id, name))
		}
		return cut(lines, start, end), nil
	})
}

func folderRef(id, name string) string {
	if name != "" {
		return "«" + name + "»"
	}
	return id
}

func cut(lines []string, start, end int) []string {
	if start > 0 && strings.TrimSpace(lines[start-1]) == "" {
		start--
	}
	return append(lines[:start:start], lines[end:]...)
}

// single — правка таблицы, которая в файле одна ([public_channels], [manage]).
func single(header string, on bool, kv [][2]string) error {
	return edit(func(lines []string) ([]string, error) {
		start, end := table(lines, header, nil)
		if !on {
			if start < 0 {
				return lines, nil
			}
			return cut(lines, start, end), nil
		}
		if start < 0 {
			lines = appendTable(lines, header)
		}
		find := func(l []string) (int, int) { return table(l, header, nil) }
		return setKeys(lines, find, kv), nil
	})
}

// SetPublicChannels — открыть (on) или закрыть все публичные каналы на чтение.
func SetPublicChannels(on bool, note string) error {
	n := ""
	if note != "" {
		n = quote(note)
	}
	return single("[public_channels]", on, [][2]string{{"read", "true"}, {"note", n}})
}

// SetManageFolders — дать или снять право раскладывать чаты по папкам и архиву.
func SetManageFolders(on bool) error {
	return single("[manage]", on, [][2]string{{"folders", "true"}})
}
