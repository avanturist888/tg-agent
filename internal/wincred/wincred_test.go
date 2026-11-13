//go:build windows

package wincred

import (
	"crypto/rand"
	"errors"
	"fmt"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	// Credential Manager belongs to the Windows user, not HOME/AppData.
	// Each run owns its own key even when several test binaries run together.
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	target := fmt.Sprintf("tg-agent/test/wincred-roundtrip-%x", id)
	t.Cleanup(func() { _ = Delete(target) })
	if err := Set(target, "секрет:123_abc"); err != nil {
		t.Fatal(err)
	}
	got, err := Get(target)
	if err != nil || got != "секрет:123_abc" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := Delete(target); err != nil {
		t.Fatal(err)
	}
	if _, err := Get(target); !errors.Is(err, ErrNotFound) {
		t.Fatalf("после Delete: %v", err)
	}
}
