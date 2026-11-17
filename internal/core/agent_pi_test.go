package core

import (
	"context"
	"testing"
	"time"

	"tgagent/internal/inbox"
	"tgagent/internal/t3"
)

// pi-тред: прослойка называет файл своей сессии, служба сверяет его с базой
// T3 (threadInfo + currentSession) и healthy hello сводит подписки сессий
// одного треда в текущую — как у Codex при смене сессии.
func TestAgentFromPiRecognizesCurrentSession(t *testing.T) {
	s := watchEnv(t)
	const (
		path   = `C:\Users\vasis\.pi\agent\sessions\--C--Users-vasis-work-maria-shora--\2026-11-17T06-00-00-000Z_session.jsonl`
		thread = "e9c5d862-d587-4f2a-b4c7-dc5e39e5ff54"
	)
	threadInfo = func(_ context.Context, session string) (t3.SessionThread, error) {
		if session != path {
			t.Fatalf("wrong session path: %q", session)
		}
		return t3.SessionThread{ID: thread, Title: "мария"}, nil
	}
	threadOf = func(_ context.Context, session string) (string, error) {
		if session != path && session != "native-codex" {
			t.Fatalf("wrong session: %q", session)
		}
		return thread, nil
	}
	currentSession = func(_ context.Context, th string) string {
		if th != thread {
			t.Fatalf("wrong thread: %q", th)
		}
		return path
	}
	// Подписка осталась от прошлой (Codex) сессии этого же треда, и та
	// сессия перестала отвечать.
	stale := &subscription{Chats: map[string]int{"test": 7}, Dead: time.Now().Add(-time.Hour)}
	stale.Agent = inbox.Addr{Session: "native-codex", Kind: inbox.KindCodex}
	if err := editSubs(s, func(m map[string]*subscription) bool { m["native-codex"] = stale; return true }); err != nil {
		t.Fatal(err)
	}

	a := AgentFromPi(context.Background(), s, path)
	if a == nil || a.Session != path || !a.Codex() {
		t.Fatalf("current session not recognized: %+v", a)
	}
	subs := loadSubs(s)
	cur := subs[path]
	if cur == nil {
		t.Fatalf("pi session not registered, keys: %v", keysOf(subs))
	}
	if _, moved := subs["native-codex"]; moved {
		t.Fatal("stale session sub not merged into the current one")
	}
	if _, ok := cur.Chats["test"]; !ok {
		t.Fatalf("chats not merged: %+v", cur.Chats)
	}
	if !cur.Dead.IsZero() {
		t.Fatalf("dead mark survived hello: %v", cur.Dead)
	}
}

func TestAgentFromPiRejectsForeignHints(t *testing.T) {
	s := watchEnv(t)
	threadInfo = func(_ context.Context, session string) (t3.SessionThread, error) {
		switch session {
		case "known":
			return t3.SessionThread{ID: "thread-1"}, nil
		case "closed":
			return t3.SessionThread{ID: "thread-2", Closed: true}, nil
		}
		return t3.SessionThread{}, t3.NoThread
	}
	currentSession = func(_ context.Context, th string) string {
		if th == "thread-1" {
			return "another-session"
		}
		return ""
	}
	for name, path := range map[string]string{
		"unknown": "unknown",
		"closed":  "closed",
		"stale":   "known",
		"empty":   "",
	} {
		if a := AgentFromPi(context.Background(), s, path); a != nil {
			t.Fatalf("%s: hint accepted: %+v", name, a)
		}
	}
}

func keysOf(m map[string]*subscription) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
