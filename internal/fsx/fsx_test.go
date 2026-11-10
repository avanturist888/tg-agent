package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.json")
	for _, s := range []string{"[1]", "[1,2]"} {
		if err := WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(path); string(b) != s {
			t.Fatalf("%q", b)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(left) != 0 {
		t.Fatalf("остались временные: %v", left)
	}
}

func TestZeroed(t *testing.T) {
	if !Zeroed(make([]byte, 10)) || Zeroed(nil) || Zeroed([]byte{0, 1}) {
		t.Fatal("Zeroed")
	}
}
