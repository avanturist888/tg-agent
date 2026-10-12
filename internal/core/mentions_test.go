package core

import (
	"slices"
	"testing"
)

func TestFakeMentions(t *testing.T) {
	cases := map[string][]string{
		"@Светлана, посмотри":                        {"Светлана"},
		"Привет, @anna_k и @Bob":                     {"Bob"},
		"почта anna@mail.ru — не упоминание":         nil,
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
