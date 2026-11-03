package chatsfile

import (
	"os"
	"strings"
	"testing"

	"tgagent/internal/config"
)

func TestSetsRoundTrip(t *testing.T) {
	path := setup(t)
	if err := SetFolder(Folder{ID: "3", Name: "Новости", Read: true}); err != nil {
		t.Fatal(err)
	}
	if err := SetPublicChannels(true, "сводки"); err != nil {
		t.Fatal(err)
	}
	if err := SetManageFolders(true); err != nil {
		t.Fatal(err)
	}
	// права папки меняются на месте, вторая папка — по названию
	if err := SetFolder(Folder{ID: "3", Name: "Новости", Read: true, Send: true, Auto: true}); err != nil {
		t.Fatal(err)
	}
	if err := SetFolder(Folder{Name: "Работа", Read: true}); err != nil {
		t.Fatal(err)
	}
	sets, manage, err := config.LoadSets(path)
	if err != nil {
		t.Fatal(err)
	}
	if !manage.Folders || len(sets) != 3 {
		t.Fatalf("manage=%v sets=%+v", manage, sets)
	}
	if s := sets[0]; s.Key != "folder:3" || !s.Send || !s.AutoSend() || s.Name != "Новости" {
		t.Fatalf("папка 3: %+v", s)
	}
	if s := sets[1]; s.Key != "folder:name:работа" || s.Send {
		t.Fatalf("папка по названию: %+v", s)
	}
	if s := sets[2]; !s.Public || s.Note != "сводки" {
		t.Fatalf("каналы: %+v", s)
	}
	chats, _, err := config.LoadChats(path)
	if err != nil || len(chats) != 2 {
		t.Fatalf("чаты задеты: %v %v", chats, err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Count(string(raw), "[[folder]]") != 2 || !strings.Contains(string(raw), "# комментарий") {
		t.Fatalf("файл:\n%s", raw)
	}
	// закрыть
	if err := RemoveFolder("3", ""); err != nil {
		t.Fatal(err)
	}
	if err := SetPublicChannels(false, ""); err != nil {
		t.Fatal(err)
	}
	if err := SetManageFolders(false); err != nil {
		t.Fatal(err)
	}
	sets, manage, _ = config.LoadSets(path)
	if manage.Folders || len(sets) != 1 || sets[0].Name != "Работа" {
		t.Fatalf("после закрытия: %v %+v", manage, sets)
	}
	if err := RemoveFolder("", "нет такой"); err == nil {
		t.Fatal("несуществующая папка закрылась без ошибки")
	}
	// папка, записанная названием, получает id при новом решении
	if err := SetFolder(Folder{ID: "7", Name: "Работа", Read: true}); err != nil {
		t.Fatal(err)
	}
	sets, _, _ = config.LoadSets(path)
	if len(sets) != 1 || sets[0].Key != "folder:7" {
		t.Fatalf("id не дописан: %+v", sets)
	}
}
