package tgc

import (
	"reflect"
	"testing"

	"github.com/gotd/td/tg"
)

func TestStubMentions(t *testing.T) {
	known := func(id int64) (int64, bool) { return 777, id == 5 }
	got, users := stubMentions("[Света](tg://user?id=5), [Аня](tg://user?id=6) и [сайт](https://example.com)", known)
	want := "[Света](" + mentionStub + "5), [Аня](tg://user?id=6) и [сайт](https://example.com)"
	if got != want {
		t.Fatalf("заглушки:\n%s\nждали\n%s", got, want)
	}
	if len(users) != 1 || users[5] != 777 {
		t.Fatalf("люди: %v", users)
	}
}

func TestUnstub(t *testing.T) {
	// так сервер разбирает «Привет, [Света](заглушка)» и `[x](tg://user?id=5)` в коде
	blocks := []tg.PageBlockClass{
		&tg.PageBlockParagraph{Text: &tg.TextConcat{Texts: []tg.RichTextClass{
			&tg.TextPlain{Text: "Привет, "},
			&tg.TextURL{Text: &tg.TextPlain{Text: "Света"}, URL: mentionStub + "5"},
			&tg.TextFixed{Text: &tg.TextPlain{Text: "[x](" + mentionStub + "5)"}},
		}}},
		&tg.PageBlockList{Items: []tg.PageListItemClass{
			&tg.PageListItemText{Text: &tg.TextBold{Text: &tg.TextURL{Text: &tg.TextPlain{Text: "Аня"}, URL: mentionStub + "6"}}},
			&tg.PageListItemText{Text: &tg.TextURL{Text: &tg.TextPlain{Text: "сайт"}, URL: "https://example.com/"}},
		}},
	}
	found := map[int64]bool{}
	unstub(reflect.ValueOf(blocks), found)
	if !found[5] || !found[6] || len(found) != 2 {
		t.Fatalf("упомянуты: %v", found)
	}
	texts := blocks[0].(*tg.PageBlockParagraph).Text.(*tg.TextConcat).Texts
	if m, ok := texts[1].(*tg.TextMentionName); !ok || m.UserID != 5 || plainRich(m.Text) != "Света" {
		t.Fatalf("упоминание: %#v", texts[1])
	}
	if code := plainRich(texts[2].(*tg.TextFixed).Text); code != "[x](tg://user?id=5)" {
		t.Fatalf("заглушка в коде должна вернуться: %q", code)
	}
	items := blocks[1].(*tg.PageBlockList).Items
	if _, ok := items[0].(*tg.PageListItemText).Text.(*tg.TextBold).Text.(*tg.TextMentionName); !ok {
		t.Fatal("упоминание внутри жирного и списка")
	}
	if _, ok := items[1].(*tg.PageListItemText).Text.(*tg.TextURL); !ok {
		t.Fatal("обычная ссылка остаётся ссылкой")
	}
}

func TestRestoreDraft(t *testing.T) {
	if r := restoreDraft(&tg.DraftMessageEmpty{}); r.Message != "" || r.RichMessage != nil {
		t.Fatalf("пустой черновик — очистка: %+v", r)
	}
	p := &tg.DraftMessage{Message: "недописанное", NoWebpage: true}
	p.SetEntities([]tg.MessageEntityClass{&tg.MessageEntityBold{Offset: 0, Length: 3}})
	r := restoreDraft(p)
	if r.Message != "недописанное" || !r.NoWebpage || len(r.Entities) != 1 {
		t.Fatalf("черновик владельца возвращается: %+v", r)
	}
}

func TestRichMentionsRead(t *testing.T) {
	b := &Batch{Users: map[int64]*tg.User{5: {ID: 5, FirstName: "Светлана"}}, Chats: map[int64]tg.ChatClass{}}
	m := &tg.Message{}
	m.SetRichMessage(tg.RichMessage{Blocks: []tg.PageBlockClass{
		&tg.PageBlockParagraph{Text: &tg.TextConcat{Texts: []tg.RichTextClass{
			&tg.TextMentionName{Text: &tg.TextPlain{Text: "Светлана"}, UserID: 5},
			&tg.TextPlain{Text: " и "},
			&tg.TextMention{Text: &tg.TextPlain{Text: "@MariShora"}},
		}}},
	}})
	got := mentions(m, b)
	if len(got) != 2 {
		t.Fatalf("упоминаний: %d", len(got))
	}
	if id, _ := got[0].Get("id"); id != int64(5) {
		t.Fatalf("по id: %v", id)
	}
	if un, _ := got[1].Get("username"); un != "MariShora" {
		t.Fatalf("по username: %v", un)
	}
}
