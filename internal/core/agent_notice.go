package core

import (
	"context"
	"errors"
	"path/filepath"

	"tgagent/internal/config"
	"tgagent/internal/inbox"
	"tgagent/internal/t3"
)

func t3TokenPath(s *config.Settings) string {
	if s != nil {
		return filepath.Join(s.DataDir(), "t3-token.json")
	}
	return filepath.Join(config.DataPath(), "t3-token.json")
}

var t3SendThread = t3.SendThread

func sendAgentNotice(ctx context.Context, s *config.Settings, agent inbox.Addr, text string) (string, error) {
	if agent.Codex() {
		return "t3", codexNotice(ctx, s, agent, text)
	}
	// An access request can retain the old Claude address after the thread has
	// switched providers. Deliver directly to the logical T3 thread instead of
	// its former Claude inbox socket.
	if agent.Session != "" {
		if thread, err := threadOf(ctx, agent.Session); err == nil && thread != "" {
			if current := currentSession(ctx, thread); current != "" && current != agent.Session {
				return "t3", threadNotice(ctx, s, thread, text)
			}
		}
	}
	return "inbox", inbox.Send(ctx, agent, "tg-agent", text)
}

// codexNotice delivers service notices directly to the T3 Code thread for
// non-Claude providers (Codex, pi, Antigravity). A failed send leaves the
// notice pending in tg-agent for retry.
var codexNotice = func(ctx context.Context, s *config.Settings, agent inbox.Addr, text string) error {
	if !agent.Codex() || agent.Session == "" {
		return errors.New("нужна сессия Codex для служебного уведомления")
	}
	thread, err := threadOf(ctx, agent.Session)
	if err != nil {
		return err
	}
	if thread == "" {
		return t3.NoThread
	}
	return threadNotice(ctx, s, thread, text)
}

var threadNotice = func(ctx context.Context, s *config.Settings, thread, text string) error {
	return t3SendThread(ctx, t3TokenPath(s), thread, text)
}
