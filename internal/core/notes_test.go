package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tgagent/internal/config"
)

// twoAccounts — установка с основным аккаунтом и профилем work без своего бота.
func twoAccounts(t *testing.T) {
	t.Helper()
	old := config.Root
	config.Root = t.TempDir()
	t.Cleanup(func() { config.Root = old; _ = config.SetProfile("") })
	write := func(rel, body string) {
		path := filepath.Join(config.Root, rel)
		os.MkdirAll(filepath.Dir(path), 0o700)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".env", "TG_API_ID=1\nTG_API_HASH=x\nTG_BOT_TOKEN=1:main\nTG_APPROVAL_CHAT_ID=42\n")
	write("config/chats.toml", "[[chat]]\nalias = \"team\"\nid = -100123\nsend = true\n")
	write("profiles/work/.env", "TG_API_ID=1\nTG_API_HASH=x\n")
	write("profiles/work/config/chats.toml", "[[chat]]\nalias = \"client\"\nid = -100777\nsend = true\ntitle = \"Клиент\"\n")
}

func TestAccountNotes(t *testing.T) {
	twoAccounts(t)
	owner, err := AddNote("work", "Аккаунт студии: пишем от имени студии, на «вы».", noteByOwner, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AddNote("nobody", "x", noteByOwner, ""); err == nil {
		t.Fatal("заметка к несуществующему аккаунту")
	}
	if _, err := AddNote("work", "   ", noteByAgent, ""); err == nil {
		t.Fatal("пустая заметка")
	}
	s := &config.Settings{}
	out, err := AccountNoteTool(context.Background(), s, "Work", "Клиент просит счета до пятницы", "", "проект бала")
	if err != nil {
		t.Fatal(err)
	}
	notes := NotesOf("work")
	if len(notes) != 2 || notes[1].Author() != "агент (проект бала)" {
		t.Fatalf("заметки: %+v", notes)
	}
	if v, _ := out.Get("account"); v != "work" {
		t.Fatalf("ответ инструмента: %v", v)
	}
	// агент не убирает заметку владельца, свою — убирает
	if _, err := AccountNoteTool(context.Background(), s, "", "", owner.ID, ""); err == nil {
		t.Fatal("заметку владельца агент убрал")
	}
	if _, err := AccountNoteTool(context.Background(), s, "", "", notes[1].ID, ""); err != nil {
		t.Fatal(err)
	}
	// агенту из контейнера — нельзя
	if _, err := AccountNoteTool(context.Background(), &config.Settings{Agent: &config.Agent{Name: "box"}}, "", "x", "", ""); err == nil {
		t.Fatal("агент из контейнера пишет заметки")
	}
	if acct, err := DeleteNote(owner.ID, false); err != nil || acct != "work" || len(NotesOf("work")) != 0 {
		t.Fatalf("владелец убирает любую: %s %v", acct, err)
	}
	text, _ := notesMenu()
	if !strings.Contains(text, "<b>main</b>") || !strings.Contains(text, "<b>work</b>") {
		t.Fatalf("меню заметок:\n%s", text)
	}
}

func TestAutoMenuSharedBot(t *testing.T) {
	twoAccounts(t)
	labels := func(s *config.Settings) map[string]string {
		out := map[string]string{}
		for _, row := range autoKeyboard(s)["inline_keyboard"].([][]map[string]string) {
			out[row[0]["callback_data"]] = row[0]["text"]
		}
		return out
	}
	s, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	main := labels(s)
	if len(main) != 2 || main["s:team"] == "" || !strings.Contains(main["@work|s:client"], "Клиент · work/client") {
		t.Fatalf("меню основного: чаты обоих аккаунтов, чужие — с меткой: %v", main)
	}
	// служба work перерисовывает то же меню (нажатие ей передали)
	if err := config.SetProfile("work"); err != nil {
		t.Fatal(err)
	}
	ws, err := config.Load()
	if err != nil || !ws.SharedBot {
		t.Fatalf("work без своего бота: %v", err)
	}
	if got := labels(ws); len(got) != 2 || got["s:team"] == "" || got["@work|s:client"] == "" {
		t.Fatalf("меню из службы work: %v", got)
	}
	d := ws.CallbackPrefix() + "d:1:ok"
	if acct, rest := config.SplitCallback(d); acct != "work" || rest != "d:1:ok" {
		t.Fatalf("кнопка черновика work: %s", d)
	}
}
