package tgc

import (
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// Снятое правило и значок полученной с сервера папки уходят снятыми.
func TestFolderRulesClearFlags(t *testing.T) {
	roundtrip := func(f *tg.DialogFilter) *tg.DialogFilter {
		var b bin.Buffer
		if err := f.Encode(&b); err != nil {
			t.Fatal(err)
		}
		var out tg.DialogFilter
		if err := out.Decode(&b); err != nil {
			t.Fatal(err)
		}
		return &out
	}
	f := &tg.DialogFilter{ID: 5, Title: tg.TextWithEntities{Text: "A"}}
	FolderRules{Broadcasts: true, ExcludeRead: true}.apply(f)
	setEmoticon(f, "🤖")
	got := roundtrip(f)
	if !got.Broadcasts || !got.ExcludeRead || got.Emoticon != "🤖" {
		t.Fatalf("до: %+v", got)
	}
	FolderRules{Groups: true}.apply(got)
	setEmoticon(got, "")
	got = roundtrip(got)
	if got.Broadcasts || got.ExcludeRead || !got.Groups {
		t.Fatalf("правила не сняты: %+v", got)
	}
	// снятый значок уходит пустой строкой с битом: без бита Telegram
	// оставляет прежний
	if e, ok := got.GetEmoticon(); !ok || e != "" {
		t.Fatalf("значок не снят: %q %v", e, ok)
	}
}
