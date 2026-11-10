package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLastIDSkipsZeroTail(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]struct {
		body string
		want int
	}{
		"обычный":     {"1\n22\n333\n", 333},
		"хвост нулей": {"1\n22\n333\n" + strings.Repeat("\x00", 4096), 333},
		"длинный":     {strings.Repeat("117511795352424548\n", 10) + "117511795352424549\n", 117511795352424549},
		"пустой":      {"", 0},
	} {
		path := filepath.Join(dir, name+".ids")
		os.WriteFile(path, []byte(c.body), 0o600)
		if got := LastID(path); got != c.want {
			t.Errorf("%s: %d, ждали %d", name, got, c.want)
		}
	}
}
