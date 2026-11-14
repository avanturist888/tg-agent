package tgc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func richReadMessage(id int, peer int64, text string, part bool) *tg.Message {
	m := &tg.Message{ID: id, PeerID: &tg.PeerUser{UserID: peer}, Date: 100}
	m.SetRichMessage(tg.RichMessage{Part: part, Blocks: []tg.PageBlockClass{
		&tg.PageBlockParagraph{Text: &tg.TextPlain{Text: text}},
	}})
	return m
}

func TestRichReadPaths(t *testing.T) {
	for _, path := range []string{"ids", "history", "around"} {
		t.Run(path, func(t *testing.T) {
			fullText := strings.Repeat("synthetic paragraph\n", 600) + "last section"
			preview := richReadMessage(7, 42, "synthetic paragraph", true)
			full := richReadMessage(7, 42, fullText, false)
			calls := 0
			c := &Conn{Peers: &PeerCache{data: newPeerData()}}
			c.API = tg.NewClient(historySinceInvoker(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				var msgs []tg.MessageClass
				switch r := in.(type) {
				case *tg.MessagesGetMessagesRequest:
					if path != "ids" {
						t.Fatalf("unexpected RPC %T", in)
					}
					msgs = []tg.MessageClass{preview}
				case *tg.MessagesGetHistoryRequest:
					if path == "ids" {
						t.Fatalf("unexpected RPC %T", in)
					}
					msgs = []tg.MessageClass{preview}
				case *tg.MessagesGetRichMessageRequest:
					calls++
					p, ok := r.Peer.(*tg.InputPeerUser)
					if !ok || p.UserID != 42 || r.ID != 7 {
						t.Fatal("wrong full-message target")
					}
					msgs = []tg.MessageClass{full, richReadMessage(8, 99, "unrelated", false)}
				default:
					t.Fatalf("unexpected RPC %T", in)
				}
				out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{Messages: msgs, Users: []tg.UserClass{&tg.User{ID: 42}}}
				return nil
			}))
			target := Target{Kind: "user", ID: 42, Input: &tg.InputPeerUser{UserID: 42}}
			var b *Batch
			var err error
			switch path {
			case "ids":
				b, err = c.Messages(context.Background(), target, []int{7})
			case "history":
				b, err = c.History(context.Background(), target, 10, 0, 0, "", 0)
			case "around":
				b, err = c.HistoryAround(context.Background(), target, 100, 10)
			}
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || len(b.Messages) != 1 || b.Users[42] == nil {
				t.Fatal("full batch not merged correctly")
			}
			got := c.Serialize(b.Messages[0], b)
			if text, _ := got.Get("text"); text != fullText {
				t.Fatal("full rich text lost its tail")
			}
			if _, partial := got.Get("rich_partial"); partial {
				t.Fatal("full message marked partial")
			}
		})
	}
}

func TestRichReadRejectsIncompleteResult(t *testing.T) {
	for _, scenario := range []string{"rpc error", "cancelled", "missing", "wrong peer", "wrong id", "still partial", "not rich"} {
		t.Run(scenario, func(t *testing.T) {
			c := &Conn{Peers: &PeerCache{data: newPeerData()}}
			ctx := context.Background()
			if scenario == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			c.API = tg.NewClient(historySinceInvoker(func(ctx context.Context, _ bin.Encoder, out bin.Decoder) error {
				if scenario == "rpc error" {
					return errors.New("synthetic failure")
				}
				if scenario == "cancelled" {
					return ctx.Err()
				}
				m := richReadMessage(7, 42, "complete", false)
				var msgs []tg.MessageClass
				switch scenario {
				case "wrong peer":
					m.PeerID = &tg.PeerUser{UserID: 99}
				case "wrong id":
					m.ID = 8
				case "still partial":
					m = richReadMessage(7, 42, "prefix", true)
				case "not rich":
					m = &tg.Message{ID: 7, PeerID: &tg.PeerUser{UserID: 42}}
				}
				if scenario != "missing" {
					msgs = []tg.MessageClass{m}
				}
				out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{Messages: msgs}
				return nil
			}))
			preview := richReadMessage(7, 42, "prefix", true)
			b := &Batch{Messages: []tg.MessageClass{preview}}
			err := c.completeRich(ctx, Target{Kind: "user", ID: 42, Input: &tg.InputPeerUser{UserID: 42}}, b)
			if err == nil {
				t.Fatal("incomplete read succeeded")
			}
			if scenario == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation lost")
			}
			if b.Messages[0] != preview {
				t.Fatal("invalid message replaced preview")
			}
		})
	}
}

func TestRichReadLeavesCompleteAndForeignMessagesAlone(t *testing.T) {
	c := &Conn{Peers: &PeerCache{data: newPeerData()}}
	c.API = tg.NewClient(historySinceInvoker(func(context.Context, bin.Encoder, bin.Decoder) error { t.Fatal("unexpected fetch"); return nil }))
	plain := &tg.Message{ID: 1, PeerID: &tg.PeerUser{UserID: 42}, Message: "ordinary"}
	complete := richReadMessage(2, 42, "complete", false)
	foreign := richReadMessage(3, 99, "foreign preview", true)
	b := &Batch{Messages: []tg.MessageClass{plain, complete, foreign}}
	if err := c.completeRich(context.Background(), Target{Kind: "user", ID: 42}, b); err != nil {
		t.Fatal(err)
	}
	if b.Messages[0] != plain || b.Messages[1] != complete || b.Messages[2] != foreign {
		t.Fatal("unchanged messages replaced")
	}
	got := c.Serialize(foreign, &Batch{})
	if v, _ := got.Get("rich_partial"); v != true {
		t.Fatal("unresolved preview silently looks complete")
	}
}
