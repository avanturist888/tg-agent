package core

import (
	"context"
	"errors"

	"tgagent/internal/inbox"
	"tgagent/internal/t3"
)

func sendAgentNotice(ctx context.Context, agent inbox.Addr, text string) (string, error) {
	if agent.Codex() {
		return "mailbox", codexNotice(ctx, agent, text)
	}
	// An access request can retain the old Claude address after the thread has
	// switched providers. Deliver to the logical thread, not its former socket.
	// Use the mailbox for any known native-session handoff: guessing the new
	// provider from the old address could reintroduce steer for Codex.
	if agent.Session != "" {
		if thread, err := threadOf(ctx, agent.Session); err == nil && thread != "" {
			if current := currentSession(ctx, thread); current != "" && current != agent.Session {
				return "mailbox", threadNotice(ctx, thread, text)
			}
		}
	}
	return "inbox", inbox.Send(ctx, agent, "tg-agent", text)
}

// codexNotice uses the shared mailbox: the hook adds service context between
// tools; Nestor wakes an idle thread with queue delivery. A failed send leaves
// the notice pending in tg-agent rather than trying the direct T3 steer path.
// This carries service notices, not the messages from the subscribed chat.
var codexNotice = func(ctx context.Context, agent inbox.Addr, text string) error {
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
	return threadNotice(ctx, thread, text)
}

var threadNotice = func(ctx context.Context, thread, text string) error {
	exe := nestorExe()
	if exe == "" {
		return errors.New("для уведомлений Codex нужен Нестор (TG_NESTOR, PATH, ~/nestor/bin)")
	}
	return nestorSend(ctx, exe, text, "send", thread, "-")
}
