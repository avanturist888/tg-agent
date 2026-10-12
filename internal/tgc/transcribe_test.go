package tgc

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"tgagent/internal/omap"
)

func TestTranscriptCacheKeepsEngine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcripts.json")
	// старая запись — просто текст, это была расшифровка Telegram
	if err := os.WriteFile(path, []byte(`{"chat:1": "старый текст"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := TranscriptCache{Path: path}
	if got, ok := cache.Get("chat:1"); !ok || got.Text != "старый текст" || got.Engine != EngineTelegram {
		t.Fatalf("старая запись: %+v %v", got, ok)
	}
	cache.Put("chat:2", Transcript{Status: "done", Text: "новый", Engine: "gigaam-v3_e2e_rnnt", Language: "ru"})
	got, ok := cache.Get("chat:2")
	if !ok || got.Text != "новый" || got.Engine != "gigaam-v3_e2e_rnnt" || got.Language != "ru" || got.Status != "done" {
		t.Fatalf("новая запись: %+v %v", got, ok)
	}
	if old, _ := cache.Get("chat:1"); old.Text != "старый текст" {
		t.Fatal("старая запись потерялась при записи новой")
	}

	// расшифровку Telegram без проверки языка при GigaAM переделываем
	local := STT{Local: func(context.Context, string) (LocalResult, error) { return LocalResult{}, nil }}
	if old, _ := cache.Get("chat:1"); !old.Stale(local) || old.Stale(STT{}) || got.Stale(local) {
		t.Fatal("устаревшая — только старая запись Telegram и только когда есть GigaAM")
	}
	if (Transcript{Engine: EngineTelegram, Language: "en"}).Stale(local) {
		t.Fatal("Telegram по нерусской речи — не устаревшая")
	}

	msg := omap.New()
	ApplyTranscript(msg, got)
	if e, _ := msg.Get("transcript_engine"); e != "gigaam-v3_e2e_rnnt" {
		t.Fatalf("агент должен видеть, чем расшифровано: %v", e)
	}
	if l, _ := msg.Get("transcript_language"); l != "ru" {
		t.Fatalf("язык: %v", l)
	}
}
