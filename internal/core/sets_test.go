package core

import (
	"strings"
	"testing"
	"time"

	"tgagent/internal/config"
)

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 3, 15, 0, 0, 0, time.Local)
	cases := map[string]time.Time{
		"24h":              now.Add(-24 * time.Hour),
		"90m":              now.Add(-90 * time.Minute),
		"2d":               now.Add(-48 * time.Hour),
		"1w":               now.Add(-7 * 24 * time.Hour),
		"2026-11-02":       time.Date(2026, 11, 2, 0, 0, 0, 0, time.Local),
		"2026-11-02T10:00": time.Date(2026, 11, 2, 10, 0, 0, 0, time.Local),
		"2026-11-02 10:00": time.Date(2026, 11, 2, 10, 0, 0, 0, time.Local),
		"":                 {},
	}
	for in, want := range cases {
		got, err := ParseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q → %v, %v; ждали %v", in, got, err, want)
		}
	}
	if got, err := ParseSince("2026-11-02T10:00:00Z", now); err != nil || !got.Equal(time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("RFC3339: %v %v", got, err)
	}
	for _, bad := range []string{"вчера", "-2d", "0h"} {
		if _, err := ParseSince(bad, now); err == nil {
			t.Errorf("%q принят", bad)
		}
	}
}

func TestAssignSetAliases(t *testing.T) {
	s := &config.Settings{
		Chats: map[string]config.ChatRule{
			"durov":    {Alias: "durov"},                             // явный — занят
			"chat1003": {Alias: "chat1003", Via: "публичные каналы"}, // временный из группы — не занимает
		},
		Sets: []config.ChatSet{{Key: "folder:1"}, {Key: config.PublicChannelsKey, Public: true}},
	}
	st := &config.SetsState{Aliases: map[string]string{"-1005": "kept"}}
	fresh := map[string]config.SetState{
		"folder:1": {Members: []config.SetMember{
			{Peer: -1001, Title: "Новости дня", Kind: "channel"},
			{Peer: -1005, Title: "Старый", Kind: "group"},
		}},
		config.PublicChannelsKey: {Members: []config.SetMember{
			{Peer: -1003, Title: "Дуров", Username: "Durov"},
			{Peer: -1001, Title: "Новости дня", Kind: "channel"},
			{Peer: -1004, Title: "Другой", Username: "durov_2"},
		}},
	}
	assignSetAliases(s, st, fresh)
	want := map[string]string{"-1001": "novosti-dnya-channel", "-1005": "kept", "-1003": "durov-2", "-1004": "durov_2"}
	for k, v := range want {
		if st.Aliases[k] != v {
			t.Errorf("%s → %q, ждали %q (всё: %v)", k, st.Aliases[k], v, st.Aliases)
		}
	}
	// второй пересчёт ничего не меняет
	before := map[string]string{}
	for k, v := range st.Aliases {
		before[k] = v
	}
	assignSetAliases(s, st, fresh)
	for k, v := range before {
		if st.Aliases[k] != v {
			t.Errorf("alias %s сменился: %q → %q", k, v, st.Aliases[k])
		}
	}
}

func TestMemberDiff(t *testing.T) {
	was := []config.SetMember{{Peer: 1, Title: "a"}, {Peer: 2, Title: "b"}}
	now := []config.SetMember{{Peer: 2, Title: "b"}, {Peer: 3, Title: "c"}}
	added, removed := memberDiff(was, now)
	if len(added) != 1 || added[0].Peer != 3 || len(removed) != 1 || removed[0].Peer != 1 {
		t.Fatalf("added %v removed %v", added, removed)
	}
}

func TestListedRules(t *testing.T) {
	s := &config.Settings{
		Chats: map[string]config.ChatRule{
			"saved": {Alias: "saved"},
			"news":  {Alias: "news", Via: "папка «Новости»"},
			"both":  {Alias: "both", Via: "папка «Новости», публичные каналы"},
			"chan":  {Alias: "chan", Via: "публичные каналы"},
		},
		ChatOrder: []string{"saved", "news", "both", "chan"},
		Sets:      []config.ChatSet{{Key: "folder:3", Folder: "3", Name: "Новости"}, {Key: config.PublicChannelsKey, Public: true}},
	}
	aliases := func(o ListOpts) string {
		rules, err := listedRules(s, o)
		if err != nil {
			return "ошибка: " + err.Error()
		}
		var out []string
		for _, r := range rules {
			out = append(out, r.Alias)
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct {
		o    ListOpts
		want string
	}{
		{ListOpts{}, "saved"},
		{ListOpts{Set: "all"}, "saved,news,both,chan"},
		{ListOpts{Set: "Новости"}, "news,both"},
		{ListOpts{Set: "3"}, "news,both"},
		{ListOpts{Set: "public_channels"}, "both,chan"},
		{ListOpts{ActiveSince: time.Now()}, "saved,news,both,chan"},
	} {
		if got := aliases(c.o); got != c.want {
			t.Errorf("%+v → %s, ждали %s", c.o, got, c.want)
		}
	}
	if got := aliases(ListOpts{Set: "нет"}); !strings.HasPrefix(got, "ошибка") {
		t.Errorf("неизвестная группа: %s", got)
	}
}
