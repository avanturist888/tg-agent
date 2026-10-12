package tgc

import (
	"path/filepath"
	"testing"

	"github.com/gotd/td/tg"
)

func TestMentions(t *testing.T) {
	b := &Batch{Users: map[int64]*tg.User{5: {ID: 5, FirstName: "Светлана"}}, Chats: map[int64]tg.ChatClass{}}
	m := &tg.Message{Message: "Привет, Светлана и @anna!", Entities: []tg.MessageEntityClass{
		&tg.MessageEntityMentionName{Offset: 8, Length: 8, UserID: 5},
		&tg.MessageEntityMention{Offset: 19, Length: 5},
	}}
	got := mentions(m, b)
	if len(got) != 2 {
		t.Fatalf("упоминаний: %d", len(got))
	}
	if id, _ := got[0].Get("id"); id != int64(5) {
		t.Fatalf("по id: %v", id)
	}
	if text, _ := got[0].Get("text"); text != "Светлана" {
		t.Fatalf("текст упоминания: %v", text)
	}
	if un, _ := got[1].Get("username"); un != "anna" {
		t.Fatalf("по username: %v", un)
	}
	if mentions(&tg.Message{Message: "просто текст"}, b) != nil {
		t.Fatal("без упоминаний — nil")
	}
}

func TestMentionEntityFallback(t *testing.T) {
	c := &Conn{Peers: LoadPeers(filepath.Join(t.TempDir(), "peers.json"))}
	u := &tg.User{ID: 6437927917, FirstName: "Светлана"}
	u.SetAccessHash(777)
	c.Peers.Remember([]tg.UserClass{u}, nil)

	_, ents := ParseMarkdown("[Светлана](tg://user?id=6437927917), глянь")
	e := c.mentionEntity(ents[0])
	men, ok := e.(*tg.InputMessageEntityMentionName)
	if !ok {
		t.Fatalf("ссылка tg://user — упоминание: %T", e)
	}
	if in, _ := men.UserID.(*tg.InputUser); in == nil || in.UserID != 6437927917 || in.AccessHash != 777 || men.Length != 8 {
		t.Fatalf("упоминание: %+v", men)
	}
	_, ents = ParseMarkdown("[сайт](https://example.com)")
	if _, ok := c.mentionEntity(ents[0]).(*tg.MessageEntityTextURL); !ok {
		t.Fatal("обычная ссылка остаётся ссылкой")
	}
}
