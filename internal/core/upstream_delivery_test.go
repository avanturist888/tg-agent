package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tgagent/internal/config"
)

func TestUpstreamTaskSurvivesBotFailure(t *testing.T) {
	f := &fakeUpstream{value: "1"}
	s := f.install(t)
	ctx := context.Background()
	if _, err := os.Stat(filepath.Join(s.DataDir(), "upstream-delivery")); !os.IsNotExist(err) {
		t.Fatalf("fixture must start without delivery directory: %v", err)
	}
	CheckUpstream(ctx, s)
	f.take()
	wakes := 0
	upstreamWake = func(context.Context, *config.Settings, string, string) error {
		wakes++
		return nil
	}
	f.value, f.sendErr = "2", errors.New("lost Bot API response")
	if _, err := CheckUpstream(ctx, s); err != nil {
		t.Fatal(err)
	}
	m, err := upstreamNotices(s)
	if err != nil || wakes != 1 || !m["web"].Agent || !m["web"].CardPending || m["web"].TaskPending {
		t.Fatalf("task receipt/card failure: %+v wakes=%d err=%v", m["web"], wakes, err)
	}
	// Source did not change again. Its independent pending card is recoverable.
	CheckUpstream(ctx, s)
	f.sendErr = nil
	retryUpstreamNotices(ctx, s)
	if wakes != 1 {
		t.Fatalf("card retry re-delivered the accepted task: %d", wakes)
	}
	if calls := f.take(); len(calls) != 1 || !strings.Contains(calls[0], "Задание передано агенту") {
		t.Fatalf("recovered card: %q", calls)
	}
	if cards, _ := PinnedUpstream(s); len(cards) != 1 || cards[0].MessageID == 0 || !cards[0].Agent {
		t.Fatalf("visible recovered card: %+v", cards)
	}
}

func TestUpstreamReplacementRetainsResolvedCardCleanup(t *testing.T) {
	f := &fakeUpstream{}
	s := f.install(t)
	ctx := context.Background()
	upstreamWake = func(context.Context, *config.Settings, string, string) error { return nil }
	if err := sendPinned(ctx, s, "old", "web", "title", "brief"); err != nil {
		t.Fatal(err)
	}
	if _, err := EscalateUpstream(ctx, s, "web", "decision"); err != nil {
		t.Fatal(err)
	}
	original := upstreamBot
	fail := true
	upstreamBot = func(ctx context.Context, s *config.Settings, method string, payload any, out any) error {
		if fail && (method == "editMessageText" || method == "unpinChatMessage") {
			return errors.New("bot offline")
		}
		return original(ctx, s, method, payload, out)
	}
	if _, err := AckUpstream(ctx, s, "web", "resolved"); err != nil {
		t.Fatal(err)
	}
	if err := sendPinned(ctx, s, "new", "web", "title", "brief"); err != nil {
		t.Fatal(err)
	}
	m, err := upstreamNotices(s)
	if err != nil || len(m["web"].Previous) != 1 || m["web"].Previous[0].ID != 1 || !m["web"].CardPending {
		t.Fatalf("lost unresolved old presentation: %+v %v", m["web"], err)
	}
	f.take()
	fail = false
	retryUpstreamNotices(ctx, s)
	if calls := strings.Join(f.take(), "|"); !strings.Contains(calls, "unpinChatMessage:1") {
		t.Fatalf("forgot pinned old card: %q", calls)
	}
	m, err = upstreamNotices(s)
	if err != nil || len(m["web"].Previous) != 0 || m["web"].CardPending {
		t.Fatalf("cleanup still pending: %+v %v", m["web"], err)
	}
}

func TestUpstreamRecurringIdenticalOutageIsNewEvent(t *testing.T) {
	f := &fakeUpstream{value: "1"}
	s := f.install(t)
	ctx := context.Background()
	wakes := 0
	upstreamWake = func(context.Context, *config.Settings, string, string) error { wakes++; return nil }
	if _, err := CheckUpstream(ctx, s); err != nil {
		t.Fatal(err)
	}
	for episode := 0; episode < 2; episode++ {
		f.err = errors.New("identical outage")
		for range upstreamFailNotice {
			if _, err := CheckUpstream(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
		if wakes != episode+1 {
			t.Fatalf("outage %d suppressed, wakes=%d", episode, wakes)
		}
		if _, err := AckUpstream(ctx, s, "web", "recovered"); err != nil {
			t.Fatal(err)
		}
		f.err = nil
		if _, err := CheckUpstream(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUpstreamOlderWriterAcknowledgementAndReplacement(t *testing.T) {
	f := &fakeUpstream{}
	s := f.install(t)
	ctx := context.Background()
	upstreamWake = func(context.Context, *config.Settings, string, string) error { return nil }
	if err := sendPinned(ctx, s, "old", "web", "title", "brief"); err != nil {
		t.Fatal(err)
	}
	// Older SQLite builds, and the legacy rollback Replace importer, only
	// mutate this compatible table and cannot update the new notice KV.
	if err := updatePinned(s, func(m map[string]pinnedCard) { delete(m, "web") }); err != nil {
		t.Fatal(err)
	}
	if cards, err := PinnedUpstream(s); err != nil || len(cards) != 0 {
		t.Fatalf("older ack resurrected: %+v %v", cards, err)
	}
	retryUpstreamNotices(ctx, s)
	if err := updatePinned(s, func(m map[string]pinnedCard) { m["web"] = pinnedCard{ID: 900, Text: "newer legacy card"} }); err != nil {
		t.Fatal(err)
	}
	if cards, err := PinnedUpstream(s); err != nil || len(cards) != 1 || cards[0].MessageID != 900 || cards[0].Event != "" {
		t.Fatalf("newer legacy card hidden: %+v %v", cards, err)
	}
}

func TestUpstreamWakeFailureDoesNotClaimAgentHandling(t *testing.T) {
	f := &fakeUpstream{}
	s := f.install(t)
	errWake := errors.New("mailbox unavailable")
	upstreamWake = func(context.Context, *config.Settings, string, string) error { return errWake }
	ctx := context.Background()
	if err := sendPinned(ctx, s, "update", "web", "title", "brief"); err != nil {
		t.Fatal(err)
	}
	if calls := f.take(); len(calls) != 2 || strings.Contains(calls[0], "Задание передано агенту") || calls[1] != "pinChatMessage:1" {
		t.Fatalf("owner fallback: %q", calls)
	}
	errWake = nil
	retryUpstreamNotices(ctx, s)
	if calls := f.take(); strings.Join(calls, "|") != "editMessageText:1|unpinChatMessage:1" || !strings.Contains(f.lastEdit, "Задание передано агенту") {
		t.Fatalf("accepted task updates the same card: %q", calls)
	}
}

func TestUpstreamAckBeforeCardPublication(t *testing.T) {
	f := &fakeUpstream{}
	s := f.install(t)
	ctx := context.Background()
	wakes := 0
	upstreamWake = func(context.Context, *config.Settings, string, string) error {
		wakes++
		if _, err := AckUpstream(ctx, s, "web", "checked before card"); err != nil {
			t.Fatal(err)
		}
		return nil
	}
	for range 2 { // same event after an interrupted source-state save
		if err := sendPinned(ctx, s, "update", "web", "title", "brief"); err != nil {
			t.Fatal(err)
		}
	}
	if calls := f.take(); len(calls) != 0 || wakes != 1 {
		t.Fatalf("resolved task reopened: calls=%q wakes=%d", calls, wakes)
	}
	if cards, _ := PinnedUpstream(s); len(cards) != 0 {
		t.Fatalf("acknowledged event visible again: %+v", cards)
	}
}

func TestUpstreamAckDuringCardRequest(t *testing.T) {
	f := &fakeUpstream{}
	s := f.install(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstreamWake = func(context.Context, *config.Settings, string, string) error { return nil }
	original := upstreamBot
	started, release := make(chan struct{}), make(chan struct{})
	upstreamBot = func(ctx context.Context, s *config.Settings, method string, payload any, out any) error {
		if method == "sendMessage" {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return original(ctx, s, method, payload, out)
	}
	done := make(chan error, 1)
	go func() {
		done <- sendPinned(ctx, s, "update", "web", "title", "brief")
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("publisher did not stop")
		}
	})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("publisher did not start")
	}
	if _, err := AckUpstream(ctx, s, "web", "already checked"); err != nil {
		t.Error(err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("publisher did not finish")
	}
	if cards, _ := PinnedUpstream(s); len(cards) != 0 {
		t.Fatalf("late card reopened task: %+v", cards)
	}
	if !strings.Contains(f.lastEdit, "already checked") || strings.Contains(f.lastEdit, "Задание передано агенту") {
		t.Fatalf("late card ignored resolution: %q", f.lastEdit)
	}
}

func TestUpstreamEscalateWithoutPublishedCard(t *testing.T) {
	f := &fakeUpstream{sendErr: errors.New("Bot API down")}
	s := f.install(t)
	ctx := context.Background()
	upstreamWake = func(context.Context, *config.Settings, string, string) error { return nil }
	if err := sendPinned(ctx, s, "update", "web", "title", "brief"); err != nil {
		t.Fatal(err)
	}
	if _, err := EscalateUpstream(ctx, s, "web", "owner decision"); err != nil {
		t.Fatal(err)
	}
	f.sendErr = nil
	retryUpstreamNotices(ctx, s)
	if calls := f.take(); len(calls) != 2 || !strings.Contains(calls[0], "owner decision") ||
		strings.Contains(calls[0], "Задание передано агенту") || calls[1] != "pinChatMessage:1" {
		t.Fatalf("durable escalation: %q", calls)
	}
}

func TestUpstreamTaskDoesNotRequireConfiguredBot(t *testing.T) {
	f := &fakeUpstream{}
	s := f.install(t)
	s.BotToken = ""
	wakes := 0
	upstreamWake = func(context.Context, *config.Settings, string, string) error { wakes++; return nil }
	if err := sendPinned(context.Background(), s, "update", "web", "title", "brief"); err != nil {
		t.Fatal(err)
	}
	if wakes != 1 || len(f.take()) != 0 {
		t.Fatal("bot configuration blocked task or attempted a bot request")
	}
}

func TestUpstreamUnchangedCardStillRetriesPin(t *testing.T) {
	f := &fakeUpstream{}
	s := f.install(t)
	ctx := context.Background()
	upstreamWake = func(context.Context, *config.Settings, string, string) error { return errors.New("mailbox offline") }
	original := upstreamBot
	failPin := true
	upstreamBot = func(ctx context.Context, s *config.Settings, method string, payload any, out any) error {
		if method == "pinChatMessage" && failPin {
			return errors.New("pin response lost")
		}
		if method == "editMessageText" {
			return errors.New("Bad Request: message is not modified")
		}
		return original(ctx, s, method, payload, out)
	}
	if err := sendPinned(ctx, s, "update", "web", "title", "brief"); err != nil {
		t.Fatal(err)
	}
	f.take()
	failPin = false
	retryUpstreamNotices(ctx, s)
	m, err := upstreamNotices(s)
	if err != nil || m["web"].CardPending || !m["web"].Pinned || !m["web"].TaskPending {
		t.Fatalf("unchanged text blocked pin recovery: %+v %v", m["web"], err)
	}
	if calls := f.take(); len(calls) != 1 || calls[0] != "pinChatMessage:1" {
		t.Fatalf("pin was not retried: %q", calls)
	}
}

func TestUpstreamResolutionDuringPinIsRecovered(t *testing.T) {
	f := &fakeUpstream{}
	s := f.install(t)
	ctx := context.Background()
	upstreamWake = func(context.Context, *config.Settings, string, string) error { return errors.New("no mailbox") }
	original := upstreamBot
	resolved := false
	upstreamBot = func(ctx context.Context, s *config.Settings, method string, payload any, out any) error {
		if method == "pinChatMessage" && !resolved {
			resolved = true
			m, err := upstreamNotices(s)
			if err != nil || !m["web"].Pinned {
				t.Fatalf("pin intent not saved: %+v %v", m["web"], err)
			}
			if _, err := AckUpstream(ctx, s, "web", "resolved while pinning"); err != nil {
				t.Fatal(err)
			}
		}
		return original(ctx, s, method, payload, out)
	}
	if err := sendPinned(ctx, s, "update", "web", "title", "brief"); err != nil {
		t.Fatal(err)
	}
	retryUpstreamNotices(ctx, s)
	m, err := upstreamNotices(s)
	if err != nil || !m["web"].Done || m["web"].Pinned || m["web"].CardPending {
		t.Fatalf("pin/ack race not recovered: %+v %v", m["web"], err)
	}
	if calls := f.take(); !strings.Contains(strings.Join(calls, "|"), "unpinChatMessage:1") {
		t.Fatalf("resolved card remains pinned: %q", calls)
	}
}

func TestUpstreamStaleResolutionDoesNotCloseNewEvent(t *testing.T) {
	f := &fakeUpstream{}
	s := f.install(t)
	ctx := context.Background()
	upstreamWake = func(context.Context, *config.Settings, string, string) error { return nil }
	if err := sendPinned(ctx, s, "old", "web", "title", "brief"); err != nil {
		t.Fatal(err)
	}
	old, _ := PinnedUpstream(s)
	if err := sendPinned(ctx, s, "new", "web", "title", "brief"); err != nil {
		t.Fatal(err)
	}
	if err := resolveUpstreamNotice(ctx, s, "web", old[0].Event, "stale ack", true); err == nil {
		t.Fatal("stale resolution accepted")
	}
	if cards, _ := PinnedUpstream(s); len(cards) != 1 || cards[0].Event == old[0].Event {
		t.Fatalf("new event was closed: %+v", cards)
	}
}
