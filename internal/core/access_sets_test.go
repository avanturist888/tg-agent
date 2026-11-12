package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tgagent/internal/config"
)

func TestGrantSet(t *testing.T) {
	old := config.Root
	config.Root = t.TempDir()
	t.Cleanup(func() { config.Root = old })
	os.MkdirAll(filepath.Join(config.Root, "config"), 0o700)
	os.WriteFile(config.ChatsFile(), []byte("[[chat]]\nalias = \"saved\"\nid = \"me\"\nread = true\nsend = true\n"), 0o600)
	load := func() *config.Settings {
		sets, manage, err := config.LoadSets(config.ChatsFile())
		if err != nil {
			t.Fatal(err)
		}
		return &config.Settings{Sets: sets, Manage: manage}
	}
	// чат, положенный агентом, после нового решения о папке целиком открывается
	if err := config.BlockSetChat("folder:3", -1005); err != nil {
		t.Fatal(err)
	}
	folder := &accessRequest{ID: "f", Scope: scopeFolder, Folder: "3", Title: "Новости", Reason: "сводка"}
	if err := grantSet(context.Background(), load(), folder, true, false, false); err != nil {
		t.Fatal(err)
	}
	if st := config.LoadSetsState(); st.IsBlocked("folder:3", -1005) {
		t.Fatal("блок не снят")
	}
	// повторное решение с отправкой — права расширяются у той же папки
	folder.Send = true
	if err := grantSet(context.Background(), load(), folder, true, true, true); err != nil {
		t.Fatal(err)
	}
	s := load()
	if len(s.Sets) != 1 || !s.Sets[0].AutoSend() || s.Sets[0].Name != "Новости" || !strings.Contains(s.Sets[0].Note, "сводка") {
		t.Fatalf("папка: %+v", s.Sets)
	}
	if err := grantSet(context.Background(), s, &accessRequest{Scope: scopePublic, Reason: "каналы"}, true, false, false); err != nil {
		t.Fatal(err)
	}
	if err := grantSet(context.Background(), s, &accessRequest{Scope: scopeManage}, true, false, false); err != nil {
		t.Fatal(err)
	}
	s = load()
	if len(s.Sets) != 2 || !s.Sets[1].Public || s.Sets[1].Send || !s.Manage.Folders {
		t.Fatalf("каналы и право: %+v %+v", s.Sets, s.Manage)
	}

	// кнопки: у каналов и права — только «Разрешить»/«Отказать», у папки с отправкой — как у чата
	for _, scope := range []string{scopePublic, scopeManage} {
		kb := accessKeyboard(&config.Settings{}, &accessRequest{ID: "p", Scope: scope, Read: true})["inline_keyboard"].([][]map[string]string)
		if len(kb) != 2 || kb[0][0]["callback_data"] != "a:p:r" || kb[1][0]["callback_data"] != "a:p:no" {
			t.Fatalf("%s: кнопки %v", scope, kb)
		}
	}
	kb := accessKeyboard(&config.Settings{}, &accessRequest{ID: "f", Scope: scopeFolder, Read: true, Send: true})["inline_keyboard"].([][]map[string]string)
	if len(kb) != 4 || kb[0][0]["callback_data"] != "a:f:rwa" {
		t.Fatalf("папка: кнопки %v", kb)
	}
	card := setCardMarkdown(&accessRequest{Scope: scopePublic, Count: 14, Sample: []string{"A", "B"}, Reason: "сводки"}, "")
	if !strings.Contains(card, "сейчас 14") || !strings.Contains(card, "A, B, …") {
		t.Fatalf("карточка: %s", card)
	}
}

func TestManageNeedsRight(t *testing.T) {
	ctx := context.Background()
	var d *Denied
	for name, s := range map[string]*config.Settings{
		"без права": {},
		"контейнер": {Agent: &config.Agent{Name: "bala"}, Manage: config.Manage{Folders: true}},
	} {
		if _, err := UnfiledChats(ctx, s, ""); !errors.As(err, &d) {
			t.Errorf("%s: tg_unfiled_chats: %v", name, err)
		}
		if _, err := MoveChat(ctx, s, 1, "", "A"); !errors.As(err, &d) {
			t.Errorf("%s: tg_move_chat: %v", name, err)
		}
		if _, err := ArchiveChat(ctx, s, 1, true); !errors.As(err, &d) {
			t.Errorf("%s: tg_archive_chat: %v", name, err)
		}
	}
	if _, err := RequestSetAccess(ctx, &config.Settings{Agent: &config.Agent{Name: "bala"}}, SetAccessOpts{Public: true, Reason: "x"}); !errors.As(err, &d) {
		t.Errorf("контейнер просит каналы: %v", err)
	}
}
