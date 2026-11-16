package core

import (
	"testing"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/outbox"
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
