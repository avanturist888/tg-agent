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
	exe := nestorExe()
	if exe == "" {
		return errors.New("для уведомлений Codex нужен Нестор (TG_NESTOR, PATH, ~/nestor/bin)")
	}
	return nestorSend(ctx, exe, text, "send", thread, "-")
}
