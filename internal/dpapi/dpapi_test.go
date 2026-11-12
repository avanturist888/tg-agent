package dpapi

import (
	"strings"
	"testing"
)

func TestSealOpen(t *testing.T) {
	s, err := Seal("секрет-токен")
	if err != nil {
		t.Fatal(err)
	}
	if Available && (!strings.HasPrefix(s, Prefix) || strings.Contains(s, "секрет")) {
		t.Fatalf("не зашифровано: %q", s)
	}
	again, _ := Seal("секрет-токен")
	if again != s {
		t.Fatal("одно значение — разные строки: каждое сохранение меняло бы запись")
	}
	// свежий процесс: кэша нет, расшифровка — через DPAPI
	mu.Lock()
	clear(sealed)
	clear(opened)
	mu.Unlock()
	if p, err := Open(s); err != nil || p != "секрет-токен" {
		t.Fatalf("open: %q %v", p, err)
	}
	if p, _ := Open("старый-открытый"); p != "старый-открытый" {
		t.Fatal("открытый текст старой записи")
	}
	if e, _ := Seal(""); e != "" {
		t.Fatal("пустое")
	}
}
