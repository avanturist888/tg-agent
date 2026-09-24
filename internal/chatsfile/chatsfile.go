// Package chatsfile — правка config/chats.toml без потери комментариев.
//
// Файл пишет человек руками, поэтому правим построчно: меняем только
// нужные строки нужного блока [[chat]], остальное оставляем как было.
// После каждой правки файл перечитывается — сломанный TOML не сохраняется.
package chatsfile

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"tgagent/internal/config"
)

// Chat — запись белого списка для правки.
type Chat struct {
	Alias string
	Peer  any // int64 или string
	Title string
	Read  bool
	Send  bool
	Auto  bool
	Note  string
}

func quote(s string) string { return strconv.Quote(s) }

func peerValue(p any) string {
	switch v := p.(type) {
	case int64:
		return strconv.FormatInt(v, 10)
	case int:
		return strconv.Itoa(v)
	case string:
		if _, err := strconv.ParseInt(v, 10, 64); err == nil {
			return v
		}
		return quote(v)
	}
	return quote(fmt.Sprint(p))
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// Block — текст блока [[chat]] для нового чата.
func Block(c Chat) string {
	title := c.Title
	if title == "" {
		title = c.Alias
	}
	b := fmt.Sprintf("\n[[chat]]\nalias = %s\nid = %s\ntitle = %s\nread = %s\nsend = %s\n",
		quote(c.Alias), peerValue(c.Peer), quote(title), boolStr(c.Read), boolStr(c.Send))
	if c.Send && c.Auto {
		b += "auto_send = true\n"
	}
	if c.Note != "" {
		b += "note = " + quote(c.Note) + "\n"
	}
	return b
}

func load(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	return strings.SplitAfter(text, "\n"), nil
}

// save пишет файл и проверяет, что он читается; иначе возвращает старый.
func save(path string, lines []string, old []byte) error {
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600); err != nil {
		return err
	}
	if _, _, err := config.LoadChats(path); err != nil {
		_ = os.WriteFile(path, old, 0o600)
		return fmt.Errorf("правка сломала бы %s, отменена: %w", path, err)
	}
	return nil
}

// block — границы блока [[chat]] с данным alias: [start, end).
func block(lines []string, alias string) (int, int) {
	var headers []int
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			headers = append(headers, i)
		}
	}
	for n, h := range headers {
		if strings.TrimSpace(lines[h]) != "[[chat]]" {
			continue
		}
		end := len(lines)
		if n+1 < len(headers) {
			end = headers[n+1]
		}
		for i := h + 1; i < end; i++ {
			key, value, ok := strings.Cut(strings.TrimSpace(lines[i]), "=")
			if ok && strings.TrimSpace(key) == "alias" && strings.EqualFold(unquoteTOML(value), alias) {
				return h, end
			}
		}
	}
	return -1, -1
}

func unquoteTOML(v string) string {
	v = strings.TrimSpace(stripComment(v))
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	if u, err := strconv.Unquote(v); err == nil {
		return u
	}
	return v
}

func stripComment(v string) string {
	// комментарий после значения; в строках в кавычках # не трогаем
	inStr := false
	for i, ch := range v {
		switch {
		case ch == '"':
			inStr = !inStr
		case ch == '#' && !inStr:
			return v[:i]
		}
	}
	return v
}

// setKey — задать key = value внутри блока; нет строки — дописать в конец блока.
func setKey(lines []string, start, end int, key, value string) []string {
	for i := start; i < end; i++ {
		k, _, ok := strings.Cut(strings.TrimSpace(lines[i]), "=")
		if ok && strings.TrimSpace(k) == key {
			lines[i] = key + " = " + value + "\n"
			return lines
		}
	}
	// вставляем после последней непустой строки блока
	at := end
	for at > start && strings.TrimSpace(lines[at-1]) == "" {
		at--
	}
	if at > 0 && !strings.HasSuffix(lines[at-1], "\n") {
		lines[at-1] += "\n"
	}
	out := append([]string{}, lines[:at]...)
	out = append(out, key+" = "+value+"\n")
	return append(out, lines[at:]...)
}

func removeKey(lines []string, start, end int, key string) []string {
	for i := start; i < end; i++ {
		k, _, ok := strings.Cut(strings.TrimSpace(lines[i]), "=")
		if ok && strings.TrimSpace(k) == key {
			return append(lines[:i:i], lines[i+1:]...)
		}
	}
	return lines
}

// Add дописывает новый чат в конец файла.
func Add(c Chat) error {
	path := config.Root + "/config/chats.toml"
	path = strings.ReplaceAll(path, "/", string(os.PathSeparator))
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	rules, _, _ := config.LoadChats(path)
	for a := range rules {
		if strings.EqualFold(a, c.Alias) {
			return fmt.Errorf("алиас '%s' уже занят другим чатом", c.Alias)
		}
	}
	text := string(old)
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	lines := strings.SplitAfter(text+Block(c), "\n")
	return save(path, lines, old)
}

// Update переписывает права (и, если заданы, заголовок и заметку) чата.
// Права задаются целиком, а не накапливаются.
func Update(alias string, read, send, auto bool, title, note *string) error {
	path := strings.ReplaceAll(config.Root+"/config/chats.toml", "/", string(os.PathSeparator))
	old, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines, _ := load(path)
	start, end := block(lines, alias)
	if start < 0 {
		return fmt.Errorf("чата '%s' нет в config/chats.toml", alias)
	}
	lines = setKey(lines, start, end, "read", boolStr(read))
	_, end = block(lines, alias)
	lines = setKey(lines, start, end, "send", boolStr(send))
	_, end = block(lines, alias)
	if send && auto {
		lines = setKey(lines, start, end, "auto_send", "true")
	} else {
		lines = removeKey(lines, start, end, "auto_send")
	}
	if title != nil {
		_, end = block(lines, alias)
		lines = setKey(lines, start, end, "title", quote(*title))
	}
	if note != nil {
		_, end = block(lines, alias)
		if *note == "" {
			lines = removeKey(lines, start, end, "note")
		} else {
			lines = setKey(lines, start, end, "note", quote(*note))
		}
	}
	return save(path, lines, old)
}

// Remove убирает блок чата целиком — доступ закрыт.
func Remove(alias string) error {
	path := strings.ReplaceAll(config.Root+"/config/chats.toml", "/", string(os.PathSeparator))
	old, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines, _ := load(path)
	start, end := block(lines, alias)
	if start < 0 {
		return fmt.Errorf("чата '%s' нет в config/chats.toml", alias)
	}
	// заодно убираем пустую строку перед блоком
	if start > 0 && strings.TrimSpace(lines[start-1]) == "" {
		start--
	}
	lines = append(lines[:start:start], lines[end:]...)
	return save(path, lines, old)
}
