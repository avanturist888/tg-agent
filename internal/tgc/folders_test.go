package tgc

import "testing"

func TestFolderContains(t *testing.T) {
	work := Folder{Groups: true, ExcludeMuted: true, Include: []int64{42}, Exclude: []int64{-100500}}
	cases := []struct {
		name string
		d    dialogFacts
		want bool
	}{
		{"группа по правилу", dialogFacts{id: -1, group: true}, true},
		{"заглушённая группа", dialogFacts{id: -2, group: true, muted: true}, false},
		{"исключённая группа", dialogFacts{id: -100500, group: true}, false},
		{"личка не по правилу", dialogFacts{id: 7, user: true, contact: true}, false},
		{"личка добавлена явно", dialogFacts{id: 42, user: true, muted: true}, true},
		{"канал не по правилу", dialogFacts{id: -3, broadcast: true}, false},
	}
	for _, c := range cases {
		if got := work.contains(c.d); got != c.want {
			t.Errorf("%s: %v, ждали %v", c.name, got, c.want)
		}
	}
	people := Folder{Contacts: true, NonContacts: true, ExcludeArchived: true}
	if !people.contains(dialogFacts{id: 1, user: true}) || people.contains(dialogFacts{id: 2, user: true, bot: true}) {
		t.Error("люди: личка входит, бот — нет")
	}
	if people.contains(dialogFacts{id: 3, user: true, archived: true}) {
		t.Error("архив исключён")
	}
	shared := Folder{Shared: true, Groups: true, Include: []int64{-9}}
	if shared.contains(dialogFacts{id: -1, group: true}) || !shared.contains(dialogFacts{id: -9, group: true}) {
		t.Error("папка-ссылка: только явные чаты")
	}
}
