package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tgagent/internal/config"
)

// Ленты установки до переезда на state.db (data/feeds/<alias>.ids)
// переносятся целиком: и с хвостом нулей после сбоя питания, и с id MAX
// больше 2⁵³.
func TestFeedLastAfterImport(t *testing.T) {
	old := config.Root
	config.Root = t.TempDir()
	t.Cleanup(func() { config.Root = old })
	dir := filepath.Join(config.DataPath(), "feeds")
	os.MkdirAll(dir, 0o700)
	cases := map[string]struct {
		body string
		want int
	}{
		"plain":  {"1\n22\n333\n", 333},
		"zeroes": {"1\n22\n333\n" + strings.Repeat("\x00", 4096), 333},
		"long":   {strings.Repeat("117511795352424548\n", 10) + "117511795352424549\n", 117511795352424549},
		"empty":  {"", 0},
	}
	for name, c := range cases {
		os.WriteFile(filepath.Join(dir, name+".ids"), []byte(c.body), 0o600)
	}
	s := &config.Settings{}
	for name, c := range cases {
		if got := FeedLast(s, name); got != c.want {
			t.Errorf("%s: %d, ждали %d", name, got, c.want)
		}
	}
	if n, err := appendNewer(s, "plain", []int{300, 334, 335}); err != nil || n != 2 {
		t.Fatalf("дописано %d (%v), ждали 2: старее последнего не пишем", n, err)
	}
	if got := idsAfter(s, "plain", 22); len(got) != 3 || got[0] != 333 || got[2] != 335 {
		t.Fatalf("после 22: %v", got)
	}
}
