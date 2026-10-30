package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"tgagent/internal/config"
)

func TestSlug(t *testing.T) {
	cases := map[[2]string]string{
		{"Классный праздник ИИ", "group"}: "klassnyy-prazdnik-ii-chat",
		{"Мария Шора", "user"}:            "mariya-shora",
		{"News & Updates!", "channel"}:    "news-updates-channel",
		{"🔥🔥", "user"}:                    "chat",
	}
	for in, want := range cases {
		if got := slug(in[0], in[1]); got != want {
			t.Errorf("slug(%q) = %q, ждали %q", in[0], got, want)
		}
	}
	s := &config.Settings{Chats: map[string]config.ChatRule{"mariya-shora": {}, "Mariya-Shora-2": {}}}
	if got := uniqueAlias(s, "mariya-shora"); got != "mariya-shora-3" {
		t.Errorf("uniqueAlias: %q", got)
	}
}

func TestGrantAccess(t *testing.T) {
	old := config.Root
	config.Root = t.TempDir()
	t.Cleanup(func() { config.Root = old })
	os.MkdirAll(filepath.Join(config.Root, "config"), 0o700)
	os.WriteFile(config.ChatsFile(), []byte("[[chat]]\nalias = \"saved\"\nid = \"me\"\ntitle = \"Избранное\"\nread = true\nsend = true\n"), 0o600)
	load := func() *config.Settings {
		rules, order, err := config.LoadChats(config.ChatsFile())
		if err != nil {
			t.Fatal(err)
		}
		return &config.Settings{Chats: rules, ChatOrder: order}
	}

	r := &accessRequest{ID: "x", Peer: -100777, Title: "Мария Шора", Kind: "group", Reason: "задача по отчётам"}
	alias, err := grantAccess(load(), r, true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	s := load()
	rule, ok := s.Chats[alias]
	if alias != "mariya-shora-chat" || !ok || !rule.Read || rule.Send || rulePeerID(rule) != -100777 || rule.Note == "" {
		t.Fatalf("новый чат: alias %q, %+v", alias, rule)
	}

	// повторный запрос — на отправку: права расширяются у того же alias
	again, err := grantAccess(s, r, true, true, false)
	if err != nil || again != alias {
		t.Fatalf("расширение прав: %q %v", again, err)
	}
	if rule := load().Chats[alias]; !rule.Read || !rule.Send || rule.AutoSend() {
		t.Fatalf("права после расширения: %+v", rule)
	}
	// «⚡ без подтверждения» — автоотправка решена при допуске: и у нового чата, и у известного
	again, err = grantAccess(load(), r, true, true, true)
	if rule := load().Chats[alias]; err != nil || again != alias || !rule.AutoSend() {
		t.Fatalf("автоотправка у известного: %+v %v", rule, err)
	}
	r2 := &accessRequest{ID: "y", Peer: 555, Title: "Пётр", Kind: "user", Send: true, Read: true}
	a2, err := grantAccess(load(), r2, true, true, true)
	if rule := load().Chats[a2]; err != nil || !rule.AutoSend() {
		t.Fatalf("автоотправка у нового: %+v %v", rule, err)
	}
	kb := accessKeyboard(&config.Settings{}, r2)["inline_keyboard"].([][]map[string]string)
	if len(kb) != 4 || kb[0][0]["callback_data"] != "a:y:rwa" || kb[1][0]["callback_data"] != "a:y:rw" ||
		kb[2][0]["callback_data"] != "a:y:r" || kb[3][0]["callback_data"] != "a:y:no" {
		t.Fatalf("кнопки: %v", kb)
	}
}

func TestFoldersClosedForRemoteAgents(t *testing.T) {
	s := &config.Settings{Agent: &config.Agent{Name: "bala"}}
	ctx := context.Background()
	var d *Denied
	if _, err := ListFolders(ctx, s); !errors.As(err, &d) {
		t.Errorf("tg_list_folders: %v", err)
	}
	if _, err := FolderChats(ctx, s, "Работа"); !errors.As(err, &d) {
		t.Errorf("tg_folder_chats: %v", err)
	}
	if _, err := RequestAccess(ctx, s, 1, true, false, "нужно"); !errors.As(err, &d) {
		t.Errorf("tg_request_access: %v", err)
	}
	if _, err := WaitAccess(ctx, s, "x", 1); !errors.As(err, &d) {
		t.Errorf("tg_wait_access: %v", err)
	}
}
