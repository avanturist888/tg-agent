package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"tgagent/internal/config"
	"tgagent/internal/outbox"
	"tgagent/internal/tgc"
)

func TestSavedFileCleanupGuards(t *testing.T) {
	s := watchEnv(t)
	s.Chats["saved"] = config.ChatRule{Alias: "saved", Peer: "me", Read: true, Send: true}
	box := outbox.New(s.DataDir())
	create := func(chat, text string, count int, sent bool, age time.Duration) string {
		t.Helper()
		var files []outbox.FileSnap
		for range count {
			files = append(files, outbox.FileSnap{Path: "synthetic.mp3", Name: "synthetic.mp3", Size: 1, Kind: outbox.KindAudio})
		}
		d, err := box.Create(outbox.CreateOpts{Chat: chat, Text: text, TTLMin: 120, Files: files})
		if err != nil {
			t.Fatal(err)
		}
		if sent {
			if _, err := box.MarkSent(d.ID, 123, time.Now().Add(-age).Format(time.RFC3339)); err != nil {
				t.Fatal(err)
			}
		}
		return d.ID
	}
	good := create("saved", "", 1, true, 0)
	if d, _, err := savedFileDraft(s, good); err != nil || d.ID != good {
		t.Fatalf("own recent single file refused: %v", err)
	}
	for _, id := range []string{
		create("test", "", 1, true, 0),
		create("saved", "separate text", 1, true, 0),
		create("saved", "", 2, true, 0),
		create("saved", "", 0, true, 0),
		create("saved", "", 1, false, 0),
		create("saved", "", 1, true, 2*time.Hour),
		"missing",
	} {
		if _, _, err := savedFileDraft(s, id); err == nil {
			t.Fatal("unsafe cleanup accepted")
		}
	}
	s.Agent = &config.Agent{Name: "synthetic"}
	if _, _, err := savedFileDraft(s, good); err == nil {
		t.Fatal("container cleanup accepted")
	}
	s.Agent = nil
	s.Network = config.NetWA
	if _, _, err := savedFileDraft(s, good); err == nil {
		t.Fatal("non-Telegram cleanup accepted")
	}
}

type cleanupInvoker func(context.Context, bin.Encoder, bin.Decoder) error

func (f cleanupInvoker) Invoke(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
	return f(ctx, in, out)
}

func TestSavedCleanupChecksActualMessagePeer(t *testing.T) {
	for _, scenario := range []string{"self", "other dialog", "missing", "read error"} {
		t.Run(scenario, func(t *testing.T) {
			deletes := 0
			c := &tgc.Conn{Peers: tgc.LoadPeers("")}
			c.API = tg.NewClient(cleanupInvoker(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				switch req := in.(type) {
				case *tg.MessagesGetMessagesRequest:
					if scenario == "read error" {
						return errors.New("synthetic read failure")
					}
					peer := int64(1)
					if scenario == "other dialog" {
						peer = 2
					}
					var messages []tg.MessageClass
					if scenario != "missing" {
						messages = []tg.MessageClass{&tg.Message{ID: 123, PeerID: &tg.PeerUser{UserID: peer}}}
					}
					out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{Messages: messages}
				case *tg.MessagesDeleteMessagesRequest:
					deletes++
					if len(req.ID) != 1 || req.ID[0] != 123 {
						t.Fatal("deleted another ID")
					}
					*out.(*tg.MessagesAffectedMessages) = tg.MessagesAffectedMessages{}
				default:
					t.Fatalf("unexpected RPC %T", in)
				}
				return nil
			}))
			err := cleanupSavedMessage(context.Background(), c, tgc.Target{Kind: "self", ID: 1, Input: &tg.InputPeerSelf{}}, 123)
			if scenario == "self" {
				if err != nil || deletes != 1 {
					t.Fatalf("own Saved message: %v, delete calls %d", err, deletes)
				}
			} else if err == nil || deletes != 0 {
				t.Fatalf("unverified message was deleted: %v, delete calls %d", err, deletes)
			}
		})
	}
}
