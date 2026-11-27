package tgc

import (
	"context"
	"reflect"
	"testing"

	"github.com/gotd/td/bin"
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

func TestRichInputPreservesBreaks(t *testing.T) {
	var c *Conn
	in, err := c.richInput(context.Background(), "Что уже готово:\n— пункт 1\n— пункт 2")
	if err != nil {
		t.Fatalf("richInput: %v", err)
	}
	md, ok := in.(*tg.InputRichMessageMarkdown)
	if !ok {
		t.Fatalf("ждали *tg.InputRichMessageMarkdown, получили %T", in)
	}
	want := "Что уже готово:  \n— пункт 1  \n— пункт 2"
	if md.Markdown != want {
		t.Fatalf("Markdown = %q, ждали %q", md.Markdown, want)
	}
}

func TestRichInputEscapesDollars(t *testing.T) {
	var c *Conn
	in, err := c.richInput(context.Background(), "Цена $100 и $200, формула $x + y = z$")
	if err != nil {
		t.Fatalf("richInput: %v", err)
	}
	md, ok := in.(*tg.InputRichMessageMarkdown)
	if !ok {
		t.Fatalf("ждали *tg.InputRichMessageMarkdown, получили %T", in)
	}
	want := `Цена \$100 и \$200, формула $x + y = z$`
	if md.Markdown != want {
		t.Fatalf("Markdown = %q, ждали %q", md.Markdown, want)
	}
}

type fakeDraftInvoker struct {
	savedMarkdown string
	dialogsCalls  int
}

func (f *fakeDraftInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	switch req := input.(type) {
	case *tg.MessagesGetPeerDialogsRequest:
		f.dialogsCalls++
		var draft tg.DraftMessageClass = &tg.DraftMessageEmpty{}
		if f.dialogsCalls > 1 {
			dm := &tg.DraftMessage{}
			dm.SetRichMessage(tg.RichMessage{Blocks: []tg.PageBlockClass{
				&tg.PageBlockParagraph{Text: &tg.TextURL{
					Text: &tg.TextPlain{Text: "Света"},
					URL:  mentionStub + "5",
				}},
			}})
			draft = dm
		}
		var b bin.Buffer
		resp := &tg.MessagesPeerDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{
			Peer:  &tg.PeerUser{UserID: 1},
			Draft: draft,
		}}}
		if err := resp.Encode(&b); err != nil {
			return err
		}
		return output.Decode(&b)
	case *tg.MessagesSaveDraftRequest:
		if rm, ok := req.GetRichMessage(); ok {
			if md, ok := rm.(*tg.InputRichMessageMarkdown); ok {
				f.savedMarkdown = md.Markdown
			}
		}
		var b bin.Buffer
		if err := (&tg.BoolTrue{}).Encode(&b); err != nil {
			return err
		}
		return output.Decode(&b)
	}
	return nil
}

func TestRichInputStubMentionsGetsPreservedBreaks(t *testing.T) {
	inv := &fakeDraftInvoker{}
	peers := LoadPeers("")
	peers.data.Users[5] = 777
	c := &Conn{
		API:   tg.NewClient(inv),
		Peers: peers,
	}

	in, err := c.richInput(context.Background(), "Привет, [Света](tg://user?id=5)\n— пункт 1\n— пункт 2")
	if err != nil {
		t.Fatalf("richInput: %v", err)
	}
	rich, ok := in.(*tg.InputRichMessage)
	if !ok {
		t.Fatalf("ждали *tg.InputRichMessage, получили %T", in)
	}
	wantSaved := "Привет, [Света](" + mentionStub + "5)  \n— пункт 1  \n— пункт 2"
	if inv.savedMarkdown != wantSaved {
		t.Fatalf("parseRich получил:\n%q\nждали:\n%q", inv.savedMarkdown, wantSaved)
	}
	if len(rich.Users) != 1 {
		t.Fatalf("ждали 1 пользователя в rich.Users, получили %d", len(rich.Users))
	}
}
