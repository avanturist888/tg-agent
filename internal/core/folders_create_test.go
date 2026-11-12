package core

import (
	"context"
	"errors"
	"testing"

	"tgagent/internal/config"
	"tgagent/internal/tgc"
)

func TestFolderToolsNeedRight(t *testing.T) {
	ctx := context.Background()
	var d *Denied
	for name, s := range map[string]*config.Settings{
		"без права": {},
		"контейнер": {Agent: &config.Agent{Name: "bala"}, Manage: config.Manage{Folders: true}},
	} {
		if _, err := CreateFolder(ctx, s, FolderOpts{Title: "A", Chats: []int64{1}}); !errors.As(err, &d) {
			t.Errorf("%s: tg_create_folder: %v", name, err)
		}
		if _, err := EditFolder(ctx, s, "A", FolderOpts{Title: "B"}); !errors.As(err, &d) {
			t.Errorf("%s: tg_edit_folder: %v", name, err)
		}
		if _, err := DeleteFolder(ctx, s, "A"); !errors.As(err, &d) {
			t.Errorf("%s: tg_delete_folder: %v", name, err)
		}
	}
}

// Новая папка и новое название не совпадают с правилами владельца: иначе
// агент собрал бы себе открытую папку.
func TestFolderOwnerRulesUntouchable(t *testing.T) {
	s := &config.Settings{Sets: []config.ChatSet{
		{Key: config.FolderKey("", "Работа"), Name: "Работа", Read: true},
		{Key: config.FolderKey("2", ""), Folder: "2", Read: true},
		{Key: config.PublicChannelsKey, Public: true, Read: true},
	}}
	folders := []tgc.Folder{{ID: 3, Title: "Семья"}, {ID: 4, Title: "Работа"}}
	var d *Denied
	if err := titleFree(s, folders, "работа", -1); !errors.As(err, &d) {
		t.Fatalf("название из правила владельца: %v", err)
	}
	var b *Bad
	if err := titleFree(s, folders, "семья", -1); !errors.As(err, &b) {
		t.Fatalf("занятое название: %v", err)
	}
	if err := titleFree(s, folders, "Семья", 3); err != nil {
		t.Fatalf("своё же название: %v", err)
	}
	if err := titleFree(s, folders, "Новости", -1); err != nil {
		t.Fatalf("свободное название: %v", err)
	}
	if err := titleFree(s, folders, "tg-agent тест", -1); !errors.As(err, &b) {
		t.Fatalf("13 символов: %v", err)
	}
	if err := titleFree(s, folders, "Непрочитанны", -1); err != nil {
		t.Fatalf("12 символов: %v", err)
	}
	// 2 — в правиле владельца (папки нет), 3 и 4 заняты
	if id, err := freeFolderID(s, folders); err != nil || id != 5 {
		t.Fatalf("номер папки: %d %v", id, err)
	}
	if !ownerFolder(s, folders[1]) || ownerFolder(s, folders[0]) || !ownerFolder(s, tgc.Folder{ID: 2, Title: "x"}) {
		t.Fatal("ownerFolder")
	}
	ctx := context.Background()
	s.Manage.Folders = true
	if _, err := CreateFolder(ctx, s, FolderOpts{Title: "A"}); !errors.As(err, &b) {
		t.Fatalf("пустая папка: %v", err)
	}
	if _, err := EditFolder(ctx, s, "A", FolderOpts{Hide: []int64{1}, Unhide: []int64{1}}); !errors.As(err, &b) {
		t.Fatalf("hide и unhide разом: %v", err)
	}
	if _, err := EditFolder(ctx, s, "A", FolderOpts{}); !errors.As(err, &b) {
		t.Fatalf("нечего менять: %v", err)
	}
}

func TestFolderRulesParse(t *testing.T) {
	r, any, err := parseFolderRules([]string{"groups", " Channels "}, []string{"archived"})
	if err != nil || !any || !r.Groups || !r.Broadcasts || !r.ExcludeArchived || r.Contacts {
		t.Fatalf("%+v %v %v", r, any, err)
	}
	if _, any, _ := parseFolderRules(nil, []string{"muted"}); any {
		t.Fatal("одни исключения — не правило")
	}
	var b *Bad
	if _, _, err := parseFolderRules([]string{"каналы"}, nil); !errors.As(err, &b) {
		t.Fatalf("незнакомое правило: %v", err)
	}
}

// Папка агента — по номеру и названию: владелец переименовал её или завёл
// свою под тем же номером — она уже не агента.
func TestMadeByAgent(t *testing.T) {
	old := config.Root
	config.Root = t.TempDir()
	t.Cleanup(func() { config.Root = old })
	ctx := context.Background()
	s := &config.Settings{}
	a := tgc.Folder{ID: 5, Title: "A"}
	agentMadeOf := func(ctx context.Context, s *config.Settings, folders []tgc.Folder) map[int]bool {
		_, out, err := listedFolders(ctx, s, func() ([]tgc.Folder, error) { return folders, nil })
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if agentMadeOf(ctx, s, []tgc.Folder{a})[5] {
		t.Fatal("без записи")
	}
	if err := withFolders(ctx, s, func(b *agentBook) error { b.put(5, "A"); b.put(6, "Б"); return nil }); err != nil {
		t.Fatal(err)
	}
	if got := agentMadeOf(ctx, s, []tgc.Folder{a, {ID: 6, Title: "Б"}}); !got[5] || !got[6] {
		t.Fatalf("свои папки: %v", got)
	}
	// владелец переименовал 5 в «B», 6 удалил: записи снимаются насовсем
	agentMadeOf(ctx, s, []tgc.Folder{{ID: 5, Title: "B"}})
	if got := agentMadeOf(ctx, s, []tgc.Folder{a, {ID: 6, Title: "Б"}}); got[5] || got[6] {
		t.Fatalf("вернули название — папка снова агента: %v", got)
	}
	if len(loadAgentFolders()) != 0 {
		t.Fatalf("учёт: %+v", loadAgentFolders())
	}
}
