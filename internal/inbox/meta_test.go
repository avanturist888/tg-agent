package inbox

import (
	"context"
	"encoding/json"
	"testing"
)

func TestFromMetaCodex(t *testing.T) {
	// образец _meta вызова из треда Codex в T3 (10.11.2026)
	raw := `{"callId":"exec-1","progressToken":1,"sessionId":"01a1c70e-b087-7da3-831f-3842f750ab3a",
		"threadId":"01a1c70e-b087-7da3-831f-3842f750ab3a","windowId":"01a1c70e-b087-7da3-831f-3842f750ab3a:0",
		"x-codex-turn-metadata":{"thread_id":"01a1c70e-b087-7da3-831f-3842f750ab3a","turn_id":"01a1c70e-ba79-7ba3-b52f-7942b3eb0586"}}`
	var meta map[string]any
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatal(err)
	}
	c := FromMeta(meta)
	if len(c) != 1 || c[0].Session != "01a1c70e-b087-7da3-831f-3842f750ab3a" || !c[0].Codex() || c[0].Key() != c[0].Session {
		t.Fatalf("%+v", c)
	}
	// номер треда только внутри JSON-строки
	c = FromMeta(map[string]any{"x-codex-turn-metadata": `{"thread_id":"01a1c710-9cc2-7bc1-9938-e176f452800b"}`})
	if len(c) != 1 || c[0].Session != "01a1c710-9cc2-7bc1-9938-e176f452800b" {
		t.Fatalf("из строки: %+v", c)
	}
	if FromMeta(map[string]any{"progressToken": 1}) != nil || FromMeta(nil) != nil {
		t.Fatal("не Codex принят за Codex")
	}
	if err := Send(context.Background(), *c[0], "x", "y"); err != ErrNoSocket {
		t.Fatalf("Send без канала: %v", err)
	}
}

func TestFromMetaAntigravity(t *testing.T) {
	const (
		convID = "26603355-d2b1-4e80-8e41-ee6f17396c36"
		rootID = "8b46a731-faea-4385-a40c-eec24d9a5712"
	)
	c := FromMeta(map[string]any{"antigravity.google/conversation_id": convID})
	if len(c) != 1 || c[0].Session != convID || !c[0].Codex() || c[0].Key() != convID {
		t.Fatalf("conversation_id: %+v", c)
	}
	// при наличии обоих ключей точная беседа первой, запасной root — следом
	c = FromMeta(map[string]any{
		"antigravity.google/root_conversation_id": rootID,
		"antigravity.google/conversation_id":      convID,
	})
	if len(c) != 2 || c[0].Session != convID || c[1].Session != rootID {
		t.Fatalf("приоритет conversation_id над root: %+v", c)
	}
	// запасной root работает, когда основной отсутствует или не UUID
	c = FromMeta(map[string]any{
		"antigravity.google/conversation_id":      "not-a-uuid",
		"antigravity.google/root_conversation_id": rootID,
	})
	if len(c) != 1 || c[0].Session != rootID || !c[0].Codex() {
		t.Fatalf("запасной root_conversation_id: %+v", c)
	}
	// не-UUID значение игнорируется
	if c := FromMeta(map[string]any{"antigravity.google/conversation_id": "not-a-uuid"}); c != nil {
		t.Fatalf("не-UUID принят: %+v", c)
	}
	// Antigravity различает наличие/отсутствие ключей верхнего уровня
	if !Antigravity(map[string]any{"antigravity.google/conversation_id": convID}) {
		t.Fatal("conversation_id не узнан как Antigravity")
	}
	if !Antigravity(map[string]any{"antigravity.google/root_conversation_id": rootID}) {
		t.Fatal("root_conversation_id не узнан как Antigravity")
	}
	if !Antigravity(map[string]any{"antigravity.google/conversation_id": "not-a-uuid"}) {
		t.Fatal("непустая строка conversation_id должна узнаваться для подсказки")
	}
	for name, meta := range map[string]map[string]any{
		"nil":        nil,
		"empty":      {},
		"codex":      {"threadId": convID},
		"empty_val":  {"antigravity.google/conversation_id": ""},
		"non_string": {"antigravity.google/conversation_id": 123},
	} {
		if Antigravity(meta) {
			t.Fatalf("%s: ошибочно узнан как Antigravity", name)
		}
	}
}
