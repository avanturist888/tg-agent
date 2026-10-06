package tgc

import (
	"testing"

	"github.com/gotd/td/tg"
)

func header(msgID, topID int, forum bool) *tg.MessageReplyHeader {
	h := &tg.MessageReplyHeader{ForumTopic: forum}
	h.SetReplyToMsgID(msgID)
	if topID != 0 {
		h.SetReplyToTopID(topID)
	}
	return h
}

func TestTopicOfAndReply(t *testing.T) {
	cases := []struct {
		name         string
		m            tg.MessageClass
		topic, reply int
	}{
		{"General без ответа", &tg.Message{ID: 10}, GeneralTopic, 0},
		{"ответ в General", &tg.Message{ID: 11, ReplyTo: header(10, 0, false)}, GeneralTopic, 10},
		{"сообщение в теме", &tg.Message{ID: 12, ReplyTo: header(5, 0, true)}, 5, 0},
		{"ответ в теме", &tg.Message{ID: 13, ReplyTo: header(12, 5, true)}, 5, 12},
		{"создание темы", &tg.MessageService{ID: 5, Action: &tg.MessageActionTopicCreate{Title: "Заказы"}}, 5, 0},
	}
	for _, c := range cases {
		if got := TopicOf(c.m, true); got != c.topic {
			t.Errorf("%s: тема %d, ждали %d", c.name, got, c.topic)
		}
		if got := ReplyID(c.m); got != c.reply {
			t.Errorf("%s: ответ на %d, ждали %d", c.name, got, c.reply)
		}
	}
	if TopicOf(&tg.Message{ID: 1, ReplyTo: header(5, 0, true)}, false) != 0 {
		t.Error("не форум — темы нет")
	}
}

func TestReplyToTopic(t *testing.T) {
	id := func(v int64) *int64 { return &v }
	get := func(r tg.InputReplyToClass) (int, int) {
		m, ok := r.(*tg.InputReplyToMessage)
		if !ok {
			return -1, -1
		}
		return m.ReplyToMsgID, m.TopMsgID
	}
	if replyTo(nil, 0) != nil || replyTo(nil, GeneralTopic) != nil {
		t.Fatal("без ответа и темы (или в General) — без reply_to")
	}
	if msg, top := get(replyTo(id(7), 0)); msg != 7 || top != 0 {
		t.Fatalf("обычный ответ: %d %d", msg, top)
	}
	if msg, top := get(replyTo(nil, 5)); msg != 5 || top != 0 {
		t.Fatalf("новое сообщение в теме — ответом на тему: %d %d", msg, top)
	}
	if msg, top := get(replyTo(id(12), 5)); msg != 12 || top != 5 {
		t.Fatalf("ответ внутри темы — с top_msg_id: %d %d", msg, top)
	}
	if msg, top := get(replyTo(id(7), GeneralTopic)); msg != 7 || top != 0 {
		t.Fatalf("ответ в General: %d %d", msg, top)
	}
}
