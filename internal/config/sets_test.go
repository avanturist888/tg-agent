package config

import "testing"

func TestMergeSets(t *testing.T) {
	chats := map[string]ChatRule{
		"news": {Alias: "news", Peer: int64(-1001), Read: true},
		"dur":  {Alias: "dur", Peer: "@durov", Read: true, Send: true},
	}
	order := []string{"news", "dur"}
	sets := []ChatSet{
		{Key: "folder:3", Folder: "3", Name: "Новости", Read: true, Send: true, Auto: true, Exclude: []string{"-1004"}},
		{Key: PublicChannelsKey, Public: true, Read: true, Exclude: []string{"@spam"}},
	}
	st := SetsState{
		Sets: map[string]SetState{
			"folder:3": {Members: []SetMember{
				{Peer: -1001, Title: "явный"},          // свой [[chat]] главнее
				{Peer: -1002, Title: "в обеих группах"}, // права обеих
				{Peer: -1004, Title: "исключён"},
				{Peer: -1005, Title: "положил агент"},
			}},
			PublicChannelsKey: {Members: []SetMember{
				{Peer: -1002, Title: "в обеих группах", Username: "both"},
				{Peer: -1003, Title: "Дуров", Username: "durov"}, // явный по username
				{Peer: -1006, Title: "спам", Username: "spam"},
				{Peer: -1007, Title: "канал", Username: "chan"},
			}},
		},
		Aliases: map[string]string{"-1002": "both", "-1007": "news"}, // news занят явным
		Blocked: map[string][]int64{"folder:3": {-1005}},
	}
	got, ord := mergeSets(chats, order, sets, st)
	if len(ord) != 4 {
		t.Fatalf("order = %v", ord)
	}
	both := got["both"]
	if !both.Read || !both.Send || !both.Auto || both.Via != "папка «Новости», публичные каналы" {
		t.Fatalf("both = %+v", both)
	}
	ch := got[ord[3]]
	if ch.Peer != int64(-1007) || ch.Alias != "chat1007" || ch.Send || ch.Via != "публичные каналы" {
		t.Fatalf("канал с занятым alias = %+v", ch)
	}
	for _, r := range got {
		switch r.Peer {
		case int64(-1003), int64(-1004), int64(-1005), int64(-1006):
			t.Fatalf("не должен попасть: %+v", r)
		}
	}
	if got["news"].Via != "" {
		t.Fatal("явный чат помечен группой")
	}
}

func TestExcluded(t *testing.T) {
	set := ChatSet{Exclude: []string{"-100", "@Name", " 42 "}}
	if !set.Excluded(-100, "") || !set.Excluded(1, "name") || !set.Excluded(42, "") || set.Excluded(7, "other") {
		t.Fatal("exclude")
	}
}
