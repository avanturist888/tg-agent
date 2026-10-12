package tgc

import (
	"testing"

	"github.com/gotd/td/tg"
)

func TestForwardedFrom(t *testing.T) {
	b := &Batch{Users: map[int64]*tg.User{5: {ID: 5, FirstName: "Анна", Username: "anna"}},
		Chats: map[int64]tg.ChatClass{9: &tg.Channel{ID: 9, Title: "Новости"}}}

	if forwardedFrom(&tg.Message{ID: 1}, b) != nil {
		t.Fatal("не пересланное")
	}

	fh := tg.MessageFwdHeader{Date: 1_800_000_000}
	fh.SetFromID(&tg.PeerUser{UserID: 5})
	m := &tg.Message{ID: 2}
	m.SetFwdFrom(fh)
	got := forwardedFrom(m, b)
	from, _ := got.Get("from")
	if name, _ := from.(interface{ Get(string) (any, bool) }).Get("name"); name != "Анна" {
		t.Fatalf("автор оригинала: %v", from)
	}
	if _, ok := got.Get("date"); !ok {
		t.Fatal("дата оригинала")
	}

	hidden := tg.MessageFwdHeader{Date: 1_800_000_000}
	hidden.SetFromName("Скрытый Автор")
	m.SetFwdFrom(hidden)
	if name, _ := forwardedFrom(m, b).Get("name"); name != "Скрытый Автор" {
		t.Fatal("автор скрыл аккаунт — только имя")
	}

	post := tg.MessageFwdHeader{Date: 1_800_000_000}
	post.SetFromID(&tg.PeerChannel{ChannelID: 9})
	post.SetChannelPost(42)
	m.SetFwdFrom(post)
	got = forwardedFrom(m, b)
	if p, _ := got.Get("channel_post"); p != 42 {
		t.Fatalf("пост канала: %v", p)
	}
}
