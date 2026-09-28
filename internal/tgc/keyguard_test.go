package tgc

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"tgagent/internal/config"
)

func sessionJSON(keyID string) []byte {
	// AuthKeyID в файле gotd — base64 от []byte
	return []byte(`{"Version":1,"Data":{"DC":2,"AuthKeyID":"` + keyID + `","Salt":1}}`)
}

func guardEnv(t *testing.T) *config.Settings {
	t.Helper()
	old := config.Root
	config.Root = t.TempDir() // и журнал, и файл сессии — во временной папке
	t.Cleanup(func() { config.Root = old })
	if err := os.MkdirAll(filepath.Join(config.Root, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &config.Settings{SessionPath: filepath.Join(config.Root, "data", "session.json")}
}

func TestGuardKeepsKnownKey(t *testing.T) {
	s := guardEnv(t)
	ctx := context.Background()
	rejected := 0
	g := newGuardedStorage(s, false, func() { rejected++ })

	// первый ключ пишется свободно, повторные записи того же ключа — тоже
	for range 2 {
		if err := g.StoreSession(ctx, sessionJSON("AAAAAAAAAAA=")); err != nil {
			t.Fatal(err)
		}
	}
	// gotd сменил ключ после -404 — запись отклонена, старый ключ на месте
	if err := g.StoreSession(ctx, sessionJSON("BBBBBBBBBBB=")); err == nil {
		t.Fatal("подмена ключа прошла")
	}
	if rejected != 1 {
		t.Fatalf("rejected = %d", rejected)
	}
	raw, _ := os.ReadFile(s.SessionPath)
	if string(authKeyID(raw)) != string(authKeyID(sessionJSON("AAAAAAAAAAA="))) {
		t.Fatal("старый ключ затёрт")
	}
	// старый ключ снова работает — отметка о потере снимается
	if err := g.StoreSession(ctx, sessionJSON("AAAAAAAAAAA=")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(g.lossPath()); !os.IsNotExist(err) {
		t.Fatal("отметка о потере ключа осталась")
	}
}

func TestGuardGivesUpAfterGrace(t *testing.T) {
	s := guardEnv(t)
	ctx := context.Background()
	g := newGuardedStorage(s, false, func() {})
	if err := g.StoreSession(ctx, sessionJSON("AAAAAAAAAAA=")); err != nil {
		t.Fatal(err)
	}
	// ключ «теряется» уже дольше KeyGrace — сервер и правда его забыл
	since := time.Now().Add(-KeyGrace - time.Minute).Unix()
	if err := os.WriteFile(g.lossPath(), []byte(strconv.FormatInt(since, 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.StoreSession(ctx, sessionJSON("BBBBBBBBBBB=")); err != nil {
		t.Fatal(err)
	}
	lost, _ := filepath.Glob(s.SessionPath + ".lost-*")
	if len(lost) != 1 {
		t.Fatalf("копия старого ключа: %v", lost)
	}
}

func TestGuardLoginReplaces(t *testing.T) {
	s := guardEnv(t)
	ctx := context.Background()
	g := newGuardedStorage(s, false, nil)
	if err := g.StoreSession(ctx, sessionJSON("AAAAAAAAAAA=")); err != nil {
		t.Fatal(err)
	}
	login := newGuardedStorage(s, true, nil)
	if err := login.StoreSession(ctx, sessionJSON("BBBBBBBBBBB=")); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(s.SessionPath)
	if string(authKeyID(raw)) != string(authKeyID(sessionJSON("BBBBBBBBBBB="))) {
		t.Fatal("вход не записал новый ключ")
	}
}
