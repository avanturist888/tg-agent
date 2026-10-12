package core

import (
	"context"
	"slices"
	"testing"

	"tgagent/internal/config"
)

func TestFakeMentions(t *testing.T) {
	cases := map[string][]string{
		"@Светлана, посмотри":                        {"Светлана"},
		"Привет, @anna_k и @Bob":                     {"Bob"},
		"почта anna@mail.ru — не упоминание":         nil,
		"пиши на светлана@почта.рф":                  nil,
		"[Светлана](tg://user?id=6437927917), глянь": nil,
		"(@Иван) и «@Мария»":                         {"Иван", "Мария"},
		"@durov":                                     nil,
	}
	for text, want := range cases {
		if got := fakeMentions(text); !slices.Equal(got, want) {
			t.Errorf("%q: %v, ждали %v", text, got, want)
		}
	}
}

func TestFixMentions(t *testing.T) {
	old := recentPeople
	t.Cleanup(func() { recentPeople = old })
	recentPeople = func(context.Context, *config.Settings, config.ChatRule) []person {
		return []person{
			{ID: 6437927917, Name: "Светлана", First: "Светлана"},
			{ID: 11, Name: "Иван Петров", First: "Иван"},
			{ID: 12, Name: "Иван Сидоров", First: "Иван"},
			{ID: 13, Name: "Мария Шора", First: "Мария", Username: "maria_sh"},
		}
	}
	s, rule := &config.Settings{}, config.ChatRule{Alias: "team"}

	text, notes := fixMentions(context.Background(), s, rule,
		"@Светлана, глянь. @Иван тоже. @Мария, и пиши на anna@mail.ru. @Пётр", "markdown")
	want := "[Светлана](tg://user?id=6437927917), глянь. @Иван тоже. [Мария](tg://user?id=13), и пиши на anna@mail.ru. @Пётр"
	if text != want {
		t.Fatalf("текст:\n%s\nждали:\n%s", text, want)
	}
	status := map[string]string{}
	for _, n := range notes {
		k, _ := n.Get("text")
		v, _ := n.Get("status")
		status[k.(string)] = v.(string)
	}
	if status["@Светлана"] != "linked" || status["@Мария"] != "linked" || status["@Иван"] != "ambiguous" ||
		status["@Пётр"] != "not_found" {
		t.Fatalf("заметки: %v", status)
	}

	// без markdown ссылку не вставить — текст как есть, с подсказкой
	text, notes = fixMentions(context.Background(), s, rule, "@Светлана, привет", "plain")
	if text != "@Светлана, привет" || len(notes) != 1 {
		t.Fatalf("plain: %q %v", text, notes)
	}
	// без «@Имя» — даже не смотрим в чат
	recentPeople = func(context.Context, *config.Settings, config.ChatRule) []person {
		t.Fatal("лишний запрос к чату")
		return nil
	}
	if text, notes = fixMentions(context.Background(), s, rule, "почта anna@mail.ru", "markdown"); notes != nil {
		t.Fatalf("почта: %v", notes)
	}
}
