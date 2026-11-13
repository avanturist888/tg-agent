package tgc

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

type historySinceInvoker func(context.Context, bin.Encoder, bin.Decoder) error

func (f historySinceInvoker) Invoke(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
	return f(ctx, in, out)
}

func TestHistorySince(t *testing.T) {
	boundary := time.Date(2026, 11, 12, 5, 7, 39, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		since time.Time
		msgs  []tg.MessageClass
		want  []int
	}{
		{
			name:  "quiet chat has no messages in the requested window",
			since: boundary,
			msgs:  []tg.MessageClass{&tg.Message{ID: 4, Date: int(boundary.Add(-3 * 24 * time.Hour).Unix())}},
		},
		{
			name:  "inclusive boundary and service messages",
			since: boundary,
			msgs: []tg.MessageClass{
				&tg.Message{ID: 8, Date: int(boundary.Add(time.Hour).Unix())},
				&tg.MessageService{ID: 7, Date: int(boundary.Unix())},
				&tg.Message{ID: 6, Date: int(boundary.Add(-time.Second).Unix())},
				&tg.MessageEmpty{ID: 5},
			},
			want: []int{8, 7},
		},
		{
			name:  "filter by date even when message IDs are out of time order",
			since: boundary,
			msgs: []tg.MessageClass{
				&tg.Message{ID: 8, Date: int(boundary.Add(-time.Hour).Unix())},
				&tg.Message{ID: 7, Date: int(boundary.Add(time.Hour).Unix())},
			},
			want: []int{7},
		},
		{
			name:  "fractional duration boundary",
			since: boundary.Add(time.Millisecond),
			msgs: []tg.MessageClass{
				&tg.Message{ID: 2, Date: int(boundary.Add(time.Second).Unix())},
				&tg.Message{ID: 1, Date: int(boundary.Unix())},
			},
			want: []int{2},
		},
		{
			name: "without since preserves the original batch",
			msgs: []tg.MessageClass{&tg.Message{ID: 2, Date: 10}, &tg.MessageEmpty{ID: 1}},
			want: []int{2},
		},
		{name: "empty batch", since: boundary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Conn{Peers: LoadPeers("")}
			c.API = tg.NewClient(historySinceInvoker(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				if _, ok := in.(*tg.MessagesGetHistoryRequest); !ok {
					t.Fatalf("unexpected RPC %T", in)
				}
				out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{
					Messages: tc.msgs, Users: []tg.UserClass{&tg.User{ID: 1}},
				}
				return nil
			}))
			b, err := c.HistorySince(context.Background(), Target{}, 10, 0, 0, "", 0, tc.since)
			if err != nil {
				t.Fatal(err)
			}
			var got []int
			for _, m := range b.Messages {
				got = append(got, m.GetID())
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got message IDs %v, want %v", got, tc.want)
			}
			if b.Users[1] == nil {
				t.Fatal("message author metadata was lost")
			}
		})
	}
}

func TestHistorySinceContinuesAfterFilteredPage(t *testing.T) {
	boundary := time.Unix(1_800_000_000, 0)
	calls := 0
	c := &Conn{Peers: LoadPeers("")}
	c.API = tg.NewClient(historySinceInvoker(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		calls++
		req, ok := in.(*tg.MessagesGetHistoryRequest)
		if !ok || req.Limit != 2 {
			t.Fatalf("unexpected RPC %T", in)
		}
		var messages []tg.MessageClass
		switch calls {
		case 1:
			if req.OffsetID != 0 {
				t.Fatalf("initial cursor %d", req.OffsetID)
			}
			messages = []tg.MessageClass{&tg.Message{ID: 5, Date: 1}, &tg.Message{ID: 4, Date: 2}}
		case 2:
			if req.OffsetID != 4 {
				t.Fatalf("cursor must advance past filtered messages: %d", req.OffsetID)
			}
			messages = []tg.MessageClass{
				&tg.Message{ID: 3, Date: int(boundary.Unix())},
				&tg.Message{ID: 2, Date: int(boundary.Add(time.Second).Unix())},
			}
		default:
			t.Fatal("unexpected extra page")
		}
		out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{Messages: messages}
		return nil
	}))
	b, err := c.HistorySince(context.Background(), Target{}, 2, 0, 0, "", 0, boundary)
	if err != nil || calls != 2 || len(b.Messages) != 2 || b.Messages[0].GetID() != 3 || b.Messages[1].GetID() != 2 {
		t.Fatalf("matching messages hidden after filtered page: calls %d, batch %+v, err %v", calls, b, err)
	}
}

func TestEmptyHistorySincePreservesForum(t *testing.T) {
	c := &Conn{Peers: LoadPeers("")}
	c.API = tg.NewClient(historySinceInvoker(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		if _, ok := in.(*tg.MessagesGetHistoryRequest); !ok {
			t.Fatalf("empty forum must not fetch topic titles: %T", in)
		}
		out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{
			Messages: []tg.MessageClass{&tg.Message{ID: 1, Date: 1}},
			Chats:    []tg.ChatClass{&tg.Channel{ID: 99, Forum: true}},
		}
		return nil
	}))
	target := Target{Kind: "channel", ID: 99}
	b, err := c.HistorySince(context.Background(), target, 10, 0, 0, "", 0, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.FillTopics(context.Background(), target, b); err != nil {
		t.Fatal(err)
	}
	if len(b.Messages) != 0 || !b.Forum {
		t.Fatalf("empty time range lost forum metadata: %+v", b)
	}
}
