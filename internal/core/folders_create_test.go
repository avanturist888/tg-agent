package core

import (
	"context"
	"errors"
	"os"
	"strings"
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
		if _, err := DeleteFolder(ctx, s, "A", ""); !errors.As(err, &d) {
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

// Снятие записи не записалось на диск — процесс помнит его, и вернувшееся
// название не возвращает папку агенту.
func TestAgentBookUnsaved(t *testing.T) {
	old := config.Root
	config.Root = t.TempDir()
	t.Cleanup(func() {
		config.Root = old
		unsavedBook.list, unsavedBook.has = nil, false
	})
	ctx := context.Background()
	s := &config.Settings{}
	mine := func(folders []tgc.Folder) map[int]bool {
		_, out, err := listedFolders(ctx, s, func() ([]tgc.Folder, error) { return folders, nil })
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if err := withFolders(ctx, s, func(b *agentBook) error { b.put(5, "A"); return nil }); err != nil {
		t.Fatal(err)
	}
	// запись учёта ломается, чтение — нет: файл только для чтения (на
	// Windows rename поверх него не проходит)
	path := agentFoldersPath()
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if saveAgentFolders(nil) == nil {
		t.Skip("запись поверх файла только для чтения здесь проходит")
	}
	mine([]tgc.Folder{{ID: 5, Title: "B"}}) // владелец переименовал
	if !unsavedBook.has {
		t.Fatal("сбой записи не замечен")
	}
	if got := mine([]tgc.Folder{{ID: 5, Title: "A"}}); got[5] {
		t.Fatal("вернувшееся название вернуло папку агенту")
	}
}

// Покинуть чужую папку-ссылку — карточка с «Разрешить» / «Отказать», в
// ней сказано, что вернуть папку можно только по той же ссылке.
func TestLeaveChatlistCard(t *testing.T) {
	r := &accessRequest{ID: "l", Scope: scopeLeave, Folder: "2", Title: "Neiro", Count: 3, Sample: []string{"A", "B"},
		Reason: "владелец попросил перенести каналы в AI"}
	card := setCardMarkdown(r, "")
	for _, want := range []string{"покинуть папку-ссылку «Neiro»", "по той же ссылке", "сейчас 3: A, B, …", "перенести каналы"} {
		if !strings.Contains(card, want) {
			t.Fatalf("в карточке нет %q:\n%s", want, card)
		}
	}
	kb := accessKeyboard(&config.Settings{}, r)["inline_keyboard"].([][]map[string]string)
	if len(kb) != 2 || kb[0][0]["callback_data"] != "a:l:r" || kb[1][0]["callback_data"] != "a:l:no" {
		t.Fatalf("кнопки %v", kb)
	}
	r.Status = accessGranted
	if txt := setDecisionText(&config.Settings{}, r); !strings.Contains(txt, "покинута") {
		t.Fatalf("итог агенту: %s", txt)
	}
	var b *Bad
	s := &config.Settings{Manage: config.Manage{Folders: true}}
	if _, err := requestLeaveChatlist(context.Background(), s, tgc.Folder{ID: 2, Title: "Neiro", Shared: true}, nil, " "); !errors.As(err, &b) {
		t.Fatalf("без reason: %v", err)
	}
}

// Нажатие старой карточки не покидает другую папку-ссылку с тем же номером
// и названием: состав сверяется с тем, что владелец видел.
func TestLeaveChatlistSameFolder(t *testing.T) {
	f := tgc.Folder{ID: 2, Title: "AI", Shared: true, Include: []int64{-3, -1}, Pinned: []int64{-2}}
	r := &accessRequest{Scope: scopeLeave, Folder: "2", Title: "AI", Members: chatlistMembers(f)}
	if err := sameChatlist(f, r); err != nil {
		t.Fatalf("та же папка: %v", err)
	}
	other := f
	other.Include = []int64{-9}
	if sameChatlist(other, r) == nil {
		t.Fatal("новая папка с тем же номером и названием")
	}
	mine := f
	mine.MyInvites = true
	if sameChatlist(mine, r) == nil {
		t.Fatal("своя папка-ссылка")
	}
}
