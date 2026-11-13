package core

import (
	"context"
	"errors"

	"tgagent/internal/inbox"
)

func sendAgentNotice(ctx context.Context, agent inbox.Addr, text string) (string, error) {
	if agent.Codex() {
		return "mailbox", codexNotice(ctx, agent, text)
	}
	return "inbox", inbox.Send(ctx, agent, "tg-agent", text)
}

// codexNotice uses the shared mailbox: the hook adds service context between
// tools; Nestor wakes an idle thread. Never fall back to user/steer on failure.
// Only the service envelope (chat aliases and IDs) goes through this channel.
var codexNotice = func(ctx context.Context, agent inbox.Addr, text string) error {
	if !agent.Codex() || agent.Session == "" {
		return errors.New("нужна сессия Codex для служебного уведомления")
	}
	thread, err := threadOf(ctx, agent.Session)
	if err != nil {
		return err
	}
	exe := nestorExe()
	if exe == "" {
		return errors.New("для уведомлений Codex нужен Нестор (TG_NESTOR, PATH, ~/nestor/bin)")
	}
	return nestorSend(ctx, exe, text, "send", thread, "-")
}
