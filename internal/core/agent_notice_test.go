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
	"tgagent/internal/fsx"
	"tgagent/internal/inbox"
	"tgagent/internal/t3"
)

func TestCodexNoticeMailboxContract(t *testing.T) {
	notice := codexNotice
	mailbox := threadNotice
	watchEnv(t)
	threadNotice = mailbox
	t.Setenv("TG_NESTOR", "synthetic-nestor")
	oldSend := nestorSend
	t.Cleanup(func() { nestorSend = oldSend })
	threadOf = func(_ context.Context, session string) (string, error) {
		if session != "native-codex" {
			t.Fatalf("wrong session: %q", session)
		}
		return "mcp:target", nil
	}
	calls := 0
	var sendError error
	nestorSend = func(_ context.Context, exe, text string, args ...string) error {
		calls++
		if exe != "synthetic-nestor" || text != "tg-agent: test 11" || strings.Join(args, " ") != "send mcp:target -" {
			t.Fatalf("mailbox invocation: %q %q %q", exe, text, args)
		}
		return sendError
	}
	agent := inbox.Addr{Kind: inbox.KindCodex, Session: "native-codex"}
	if err := notice(context.Background(), agent, "tg-agent: test 11"); err != nil || calls != 1 {
		t.Fatalf("mailbox: %v, calls %d", err, calls)
	}
	sendError = errors.New("mailbox unavailable")
	if err := notice(context.Background(), agent, "tg-agent: test 11"); !errors.Is(err, sendError) {
		t.Fatalf("mailbox failure hidden: %v", err)
	}
	threadOf = func(context.Context, string) (string, error) { return "", t3.NoThread }
	if err := notice(context.Background(), agent, "tg-agent: test 11"); !errors.Is(err, t3.NoThread) || calls != 2 {
		t.Fatalf("unknown thread sent: %v, calls %d", err, calls)
	}
	threadOf = func(context.Context, string) (string, error) { return "", nil }
	if err := notice(context.Background(), agent, "tg-agent: test 11"); !errors.Is(err, t3.NoThread) || calls != 2 {
		t.Fatalf("empty target sent: %v, calls %d", err, calls)
	}
}

func TestCodexAccessNoticeRetryWithoutSteering(t *testing.T) {
	for _, handoff := range []bool{false, true} {
		name := "codex-address"
		if handoff {
			name = "saved-claude-address"
		}
		t.Run(name, func(t *testing.T) { testAccessNoticeRetry(t, handoff) })
	}
}

func testAccessNoticeRetry(t *testing.T, handoff bool) {
	t.Helper()
	watchEnv(t)
	t.Setenv("TG_API_ID", "123")
	t.Setenv("TG_API_HASH", "synthetic")
	t.Setenv("TG_NETWORK", "telegram")
	t.Setenv("TG_SEND_POLICY", "disabled")
	t.Setenv("TG_SESSION", filepath.Join(config.DataPath(), "session.json"))
	t.Setenv("TG_PROXY", "")
	if err := os.MkdirAll(config.ConfigDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsx.WriteFile(config.ChatsFile(), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	agent := inbox.Addr{Kind: inbox.KindCodex, Session: "native-codex", Socket: "stale"}
	if handoff {
		agent = inbox.Addr{Session: "old-claude", Socket: "stale", Token: "synthetic"}
		threadOf = func(_ context.Context, session string) (string, error) {
			if session != agent.Session {
				t.Fatalf("wrong saved session: %q", session)
			}
			return "logical-thread", nil
		}
		currentSession = func(_ context.Context, thread string) string {
			if thread != "logical-thread" {
				t.Fatalf("wrong logical thread: %q", thread)
			}
			return "native-codex"
		}
	}
	r := &accessRequest{ID: "synthetic-request", Title: "Synthetic chat", Status: accessDenied, Agent: &agent}
	wakeT3 = func(context.Context, *config.Settings, string, string) error {
		t.Fatal("Codex access decision fell back to user/steer")
		return nil
	}
	calls := 0
	fail := true
	accept := func(text string) error {
		calls++
		if !strings.Contains(text, r.ID) || !strings.Contains(text, "владелец отказал") {
			t.Fatalf("wrong access notice: %s", text)
		}
		if fail {
			return errors.New("mailbox unavailable")
		}
		return nil
	}
	codexNotice = func(_ context.Context, got inbox.Addr, text string) error {
		if handoff || got != agent {
			t.Fatalf("wrong Codex notice address: %+v", got)
		}
		return accept(text)
	}
	threadNotice = func(_ context.Context, thread, text string) error {
		if !handoff || thread != "logical-thread" {
			t.Fatalf("wrong handoff target: %q", thread)
		}
		return accept(text)
	}
	if tellAgentAccess(context.Background(), r) || calls != 1 {
		t.Fatalf("failed access notice acknowledged: calls %d", calls)
	}
	fail = false
	if !tellAgentAccess(context.Background(), r) || calls != 2 {
		t.Fatalf("access notice not retried: calls %d", calls)
	}
	if handoff && (r.Agent.Kind != "" || r.Agent.Session != "old-claude") {
		t.Fatal("delivery changed the saved request address")
	}
}

func TestCurrentClaudeNoticeKeepsInbox(t *testing.T) {
	watchEnv(t)
	agent := inbox.Addr{Session: "current-claude"}
	threadOf = func(context.Context, string) (string, error) { return "logical-thread", nil }
	currentSession = func(context.Context, string) string { return agent.Session }
	threadNotice = func(context.Context, string, string) error {
		t.Fatal("current Claude address was sent to the mailbox")
		return nil
	}
	via, err := sendAgentNotice(context.Background(), agent, "synthetic notice")
	if via != "inbox" || !errors.Is(err, inbox.ErrNoSocket) {
		t.Fatalf("current Claude route changed: %s, %v", via, err)
	}
}

func TestCodexNotificationRetryWithoutSteering(t *testing.T) {
	s := watchEnv(t)
	ctx := context.Background()
	agent := inbox.Addr{Kind: inbox.KindCodex, Session: "native-codex"}
	if _, err := appendNewer(s, "test", []int{10}); err != nil {
		t.Fatal(err)
	}
	if _, err := Subscribe(WithAgent(ctx, &agent), s, "test", SubOpts{After: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := appendNewer(s, "test", []int{11}); err != nil {
		t.Fatal(err)
	}
	wakeT3 = func(context.Context, *config.Settings, string, string) error {
		t.Fatal("Codex notification fell back to user/steer")
		return nil
	}
	calls := 0
	fail := true
	codexNotice = func(_ context.Context, got inbox.Addr, text string) error {
		calls++
		if got != agent || !strings.Contains(text, "after_id=10") {
			t.Fatalf("wrong notice: %+v %s", got, text)
		}
		if fail {
			return errors.New("mailbox unavailable")
		}
		return nil
	}
	notifyOnce(ctx, s, map[string]time.Time{})
	if got := loadSubs(s)[agent.Key()].Chats["test"]; got != 10 {
		t.Fatalf("failed notice lost backlog: cursor %d", got)
	}
	fail = false
	notifyOnce(ctx, s, map[string]time.Time{})
	if got := loadSubs(s)[agent.Key()].Chats["test"]; got != 11 || calls != 2 {
		t.Fatalf("notice not retried: cursor %d, calls %d", got, calls)
	}
	notifyOnce(ctx, s, map[string]time.Time{})
	if calls != 2 {
		t.Fatalf("accepted notice duplicated: %d calls", calls)
	}
}
