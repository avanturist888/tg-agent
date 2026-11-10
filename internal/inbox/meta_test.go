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
	a := FromMeta(meta)
	if a == nil || a.Session != "01a1c70e-b087-7da3-831f-3842f750ab3a" || !a.Codex() || a.Key() != a.Session {
		t.Fatalf("%+v", a)
	}
	// номер треда только внутри JSON-строки
	a = FromMeta(map[string]any{"x-codex-turn-metadata": `{"thread_id":"01a1c710-9cc2-7bc1-9938-e176f452800b"}`})
	if a == nil || a.Session != "01a1c710-9cc2-7bc1-9938-e176f452800b" {
		t.Fatalf("из строки: %+v", a)
	}
	if FromMeta(map[string]any{"progressToken": 1}) != nil || FromMeta(nil) != nil {
		t.Fatal("не Codex принят за Codex")
	}
	if err := Send(context.Background(), *a, "x", "y"); err != ErrNoSocket {
		t.Fatalf("Send без канала: %v", err)
	}
}
