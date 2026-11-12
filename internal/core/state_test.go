package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tgagent/internal/config"
	"tgagent/internal/dpapi"
)

// Подписки старой установки несут токен канала сессии открытым текстом: в
// базу он ложится зашифрованным, а сама подписка работает как прежде.
func TestLegacySubsTokenSealed(t *testing.T) {
	old := config.Root
	config.Root = t.TempDir()
	t.Cleanup(func() { config.Root = old })
	os.MkdirAll(config.DataPath(), 0o700)
	os.WriteFile(filepath.Join(config.DataPath(), "subscriptions.json"),
		[]byte(`{"s1": {"agent": {"session": "s1", "socket": "pipe-x","token": "very-secret-token"}, "chats": {"saved": 5}}}`), 0o600)
	s := &config.Settings{}
	subs := loadSubs(s)
	if subs["s1"] == nil || subs["s1"].Agent.Token != "very-secret-token" || subs["s1"].Chats["saved"] != 5 {
		t.Fatalf("подписка после переноса: %+v", subs["s1"])
	}
	db, err := stateDB(s)
	if err != nil {
		t.Fatal(err)
	}
	var body string
	if err := db.Q().QueryRow("SELECT body FROM subscriptions WHERE key = 's1'").Scan(&body); err != nil {
		t.Fatal(err)
	}
	if dpapi.Available && strings.Contains(body, "very-secret-token") {
		t.Fatalf("токен в базе открытым текстом: %s", body)
	}
}
