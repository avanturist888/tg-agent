package secret

import (
	"strings"
	"testing"
)

func TestMask(t *testing.T) {
	tok := "8123456789:AAH4kq_9-ZxYwVuTsRqPoNmLkJiHgFeDcBa"
	in := `{"text": "Use this token to access the HTTP API:\n` + tok + `\nKeep it secure", "preview": "` + tok[:30] + `"}`
	out := Mask(in)
	if strings.Contains(out, "AAH4kq") || !strings.Contains(out, "8123456789:[токен бота скрыт tg-agent]") {
		t.Fatalf("не спрятан: %s", out)
	}
	if strings.Count(out, "скрыт") != 2 {
		t.Fatalf("обрезанный токен в превью не спрятан: %s", out)
	}
	for _, keep := range []string{"10:30", "id 3606833:", "2026-11-03T20:19:20+00:00", "ratio 123456:789"} {
		if Mask(keep) != keep {
			t.Errorf("испорчено: %q → %q", keep, Mask(keep))
		}
	}
	if got := BotTokens("a " + tok + " b"); len(got) != 1 || got[0] != tok {
		t.Fatalf("BotTokens: %v", got)
	}
}
