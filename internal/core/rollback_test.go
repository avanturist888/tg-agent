package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tgagent/internal/store"
)

// Каждый старый файл объявляет, как он отдаётся старой сборке при откате:
// из базы (и тогда — как вливается обратно, Replace) или снимком (кэш).
// Новый файл без объявления не пройдёт: откат вернул бы старой сборке
// устаревшее состояние молча.
func TestLegacyRollbackDeclared(t *testing.T) {
	for _, l := range store.LegacyFiles() {
		exported := l.Export != nil || l.ExportDir != nil
		switch {
		case exported == l.Snapshot:
			t.Errorf("%s: при откате — либо из базы (Export), либо снимком (Snapshot), ровно одно", l.File)
		case exported && l.Replace == nil:
			t.Errorf("%s: выгружается из базы, но обратно вливается без Replace", l.File)
		case l.Dir && l.Export != nil:
			t.Errorf("%s: каталог выгружается ExportDir", l.File)
		}
	}
}

// Подписка уходит старой сборке с открытым токеном канала: зашифрованный
// она не знает и разбудить сессию не смогла бы.
func TestRollbackSubscriptionPlainToken(t *testing.T) {
	s := watchEnv(t)
	dir := s.DataDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := `{"k1": {"chat": "test", "agent": {"session": "s1", "socket": "pipe-x", "token": "tok-plain-123"}}}`
	if err := os.WriteFile(filepath.Join(dir, "subscriptions.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := stateDB(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ImportLegacy(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExportLegacy(dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "subscriptions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]struct {
		Agent struct {
			Token string `json:"token"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(got, &m); err != nil || m["k1"].Agent.Token != "tok-plain-123" {
		t.Fatalf("токен для старой сборки: %v %q", err, strings.TrimSpace(string(got)))
	}
}
