package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tgagent/internal/config"
)

func TestAutoMenuToggle(t *testing.T) {
	old := config.Root
	config.Root = t.TempDir()
	t.Cleanup(func() { config.Root = old })
	os.MkdirAll(filepath.Join(config.Root, "config"), 0o700)
	os.WriteFile(config.ChatsFile(), []byte(`
[[chat]]
alias = "work"
id = -100123
title = "Работа"
read = true
send = true

[[chat]]
alias = "news"
id = -100456
title = "Новости"
read = true
send = false
`), 0o600)
	load := func() *config.Settings {
		rules, order, err := config.LoadChats(config.ChatsFile())
		if err != nil {
			t.Fatal(err)
		}
		return &config.Settings{Chats: rules, ChatOrder: order}
	}

	kb := chatButtons(load(), autoView{Filter: 'a'})
	if len(kb) != 1 || kb[0]["callback_data"] != "s:work~a0" || !strings.HasPrefix(kb[0]["text"], "⚪") {
		t.Fatalf("в меню только чат с отправкой, выключенный: %v", kb)
	}
	if on, err := toggleAuto(load(), "work"); err != nil || !on || !load().Chats["work"].AutoSend() {
		t.Fatalf("включение: %v %v", on, err)
	}
	kb = chatButtons(load(), autoView{Filter: 'a'})
	if !strings.HasPrefix(kb[0]["text"], "🟢") {
		t.Fatalf("после включения: %v", kb)
	}
	if on, err := toggleAuto(load(), "work"); err != nil || on || load().Chats["work"].AutoSend() {
		t.Fatalf("выключение: %v %v", on, err)
	}
	if _, err := toggleAuto(load(), "news"); err == nil {
		t.Fatal("чат без отправки переключать нельзя")
	}
	if _, err := toggleAuto(load(), "gone"); err == nil {
		t.Fatal("чата нет")
	}
	if r := load().Chats["work"]; !r.Read || !r.Send {
		t.Fatalf("права чтения и отправки не трогаются: %+v", r)
	}
}

// chatButtons — кнопки чатов меню /auto (без строк фильтра и листания).
func chatButtons(s *config.Settings, v autoView) []map[string]string {
	_, kb := autoMenu(s, v)
	var out []map[string]string
	for _, row := range kb["inline_keyboard"].([][]map[string]string) {
		for _, b := range row {
			if !strings.HasPrefix(b["callback_data"], "am:") {
				out = append(out, b)
			}
		}
	}
	return out
}

func TestAutoMenuFilterAndPages(t *testing.T) {
	old := config.Root
	config.Root = t.TempDir()
	t.Cleanup(func() { config.Root = old })
	os.MkdirAll(filepath.Join(config.Root, "config"), 0o700)
	var toml strings.Builder
	for i := range 23 { // 23 чата с отправкой: у каждого третьего автоотправка
		fmt.Fprintf(&toml, "[[chat]]\nalias = \"c%02d\"\nid = %d\nread = true\nsend = true\nauto_send = %v\n\n", i, -100-i, i%3 == 0)
	}
	os.WriteFile(config.ChatsFile(), []byte(toml.String()), 0o600)
	rules, order, err := config.LoadChats(config.ChatsFile())
	if err != nil {
		t.Fatal(err)
	}
	s := &config.Settings{Chats: rules, ChatOrder: order, AutoSendDelaySec: 30}

	rowsOf := func(v autoView) (string, [][]map[string]string) {
		text, kb := autoMenu(s, v)
		return text, kb["inline_keyboard"].([][]map[string]string)
	}
	text, rows := rowsOf(autoView{Filter: 'a'})
	filters := rows[0]
	if len(filters) != 3 || filters[0]["text"] != "▸ Все · 23" || filters[1]["text"] != "🟢 · 8" || filters[2]["text"] != "⚪ · 15" ||
		filters[1]["callback_data"] != "am:y0" {
		t.Fatalf("фильтры: %v", filters)
	}
	nav := rows[len(rows)-1]
	if len(rows) != 1+autoPageSize+1 || len(nav) != 2 || nav[0]["text"] != "1 / 3" || nav[1]["callback_data"] != "am:a1" ||
		!strings.Contains(text, "все чаты, стр. 1 из 3") {
		t.Fatalf("первая страница: %d строк, листание %v\n%s", len(rows), nav, text)
	}
	// последняя страница: 3 чата, назад есть, вперёд нет; кнопки помнят вид
	if got := chatButtons(s, autoView{'a', 2}); len(got) != 3 || got[0]["callback_data"] != "s:c20~a2" {
		t.Fatalf("третья страница: %v", got)
	}
	_, rows = rowsOf(autoView{'a', 2})
	if nav = rows[len(rows)-1]; len(nav) != 2 || nav[0]["callback_data"] != "am:a1" || nav[1]["text"] != "3 / 3" {
		t.Fatalf("листание на последней: %v", nav)
	}
	// только включённые — одна страница, без листания; страница за краем — последняя
	got := chatButtons(s, autoView{'y', 5})
	if len(got) != 8 || got[0]["callback_data"] != "s:c00~y0" || !strings.HasPrefix(got[0]["text"], "🟢") {
		t.Fatalf("включённые: %v", got)
	}
	if text, rows = rowsOf(autoView{'y', 0}); len(rows) != 1+8 || !strings.Contains(text, "с автоотправкой.") {
		t.Fatalf("включённые без листания: %d строк\n%s", len(rows), text)
	}
	if got = chatButtons(s, autoView{'n', 1}); len(got) != 5 || strings.HasPrefix(got[0]["text"], "🟢") {
		t.Fatalf("выключенные, стр. 2: %v", got)
	}
	if v := parseAutoView("n12"); v.Filter != 'n' || v.Page != 12 {
		t.Fatalf("вид: %+v", v)
	}
	if v := parseAutoView("zz"); v.Filter != 'a' || v.Page != 0 {
		t.Fatalf("мусор: %+v", v)
	}
}
