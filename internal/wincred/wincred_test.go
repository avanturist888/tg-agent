//go:build windows

package wincred

import (
	"errors"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	const target = "tg-agent/test/wincred-roundtrip"
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
