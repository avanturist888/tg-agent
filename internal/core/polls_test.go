package core

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gotd/td/tg"

	"tgagent/internal/bot"
	"tgagent/internal/outbox"
	"tgagent/internal/tgc"
)

func TestCheckPoll(t *testing.T) {
	ok := outbox.PollSpec{Question: " Кто идёт? ", Options: []string{" я ", "не я"}}
	if err := checkPoll(&ok); err != nil || ok.Question != "Кто идёт?" || ok.Options[0] != "я" {
		t.Fatalf("обычный опрос: %v %+v", err, ok)
	}
	bad := map[string]outbox.PollSpec{
		"без вопроса":             {Options: []string{"a", "b"}},
		"один вариант":            {Question: "?", Options: []string{"a"}},
		"повтор":                  {Question: "?", Options: []string{"Да", "да"}},
		"пустой вариант":          {Question: "?", Options: []string{"a", " "}},
		"correct без викторины":   {Question: "?", Options: []string{"a", "b"}, Correct: []int{0}},
		"викторина без ответа":    {Question: "?", Options: []string{"a", "b"}, Quiz: true},
		"два верных без multiple": {Question: "?", Options: []string{"a", "b"}, Quiz: true, Correct: []int{0, 1}},
		"нет такого варианта":     {Question: "?", Options: []string{"a", "b"}, Quiz: true, Correct: []int{2}},
		"длинный вариант":         {Question: "?", Options: []string{"a", strings.Repeat("я", 101)}},
		"много вариантов":         {Question: "?", Options: strings.Split("a b c d e f g h i j k l m", " ")},
	}
	for name, p := range bad {
		if err := checkPoll(&p); err == nil {
			t.Errorf("%s: ждали отказ", name)
		}
	}
	quiz := outbox.PollSpec{Question: "2+2?", Options: []string{"3", "4", "5"}, Quiz: true, Correct: []int{1}, Solution: "просто"}
	if err := checkPoll(&quiz); err != nil {
		t.Fatalf("викторина: %v", err)
	}
}

func TestPollInfo(t *testing.T) {
	m := &tg.Message{ID: 5, Media: &tg.MessageMediaPoll{
		Poll: tg.Poll{ID: 1, PublicVoters: true, Question: tg.TextWithEntities{Text: "Кто идёт?"},
			Answers: []tg.PollAnswerClass{
				&tg.PollAnswer{Text: tg.TextWithEntities{Text: "я"}, Option: []byte{0}},
				&tg.PollAnswer{Text: tg.TextWithEntities{Text: "не я"}, Option: []byte{1}},
			}},
		Results: func() tg.PollResults {
			r := tg.PollResults{}
			r.SetResults([]tg.PollAnswerVoters{{Option: []byte{0}, Voters: 3, Chosen: true}})
			r.SetTotalVoters(3)
			return r
		}(),
	}}
	raw, err := json.Marshal(tgc.PollInfo(m))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{`"question":"Кто идёт?"`, `{"index":0,"text":"я","voters":3,"chosen":true}`,
		`{"index":1,"text":"не я","voters":0}`, `"anonymous":false`, `"total_voters":3`} {
		if !strings.Contains(got, want) {
			t.Errorf("нет %s в %s", want, got)
		}
	}
	if tgc.PollInfo(&tg.Message{ID: 6, Message: "текст"}) != nil {
		t.Error("у обычного сообщения опроса нет")
	}
}

func TestPollDraftRoundTrip(t *testing.T) {
	s := watchEnv(t)
	box := outbox.New(s.DataDir())
	p := &outbox.PollSpec{Question: "Кто идёт?", Options: []string{"я", "не я"}, Anonymous: false, Multiple: true}
	d, err := box.Create(outbox.CreateOpts{Chat: "test", TTLMin: 60, Poll: p})
	if err != nil {
		t.Fatal(err)
	}
	got, err := box.Get(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Poll == nil || got.Poll.Question != "Кто идёт?" || len(got.Poll.Options) != 2 || !got.Poll.Multiple {
		t.Fatalf("опрос не сохранился: %+v", got.Poll)
	}
	card := bot.CardMarkdown(got, "Тест", "", "")
	for _, want := range []string{"просит отправить опрос", "Кто идёт?", `1\. я`, `2\. не я`, "открытый", "несколько"} {
		if !strings.Contains(card, want) {
			t.Errorf("в карточке нет %q:\n%s", want, card)
		}
	}
}
