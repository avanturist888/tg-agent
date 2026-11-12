package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func openTemp(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeAll)
	return db
}

func TestSchemaVersion(t *testing.T) {
	db := openTemp(t)
	var v int
	if err := db.Q().QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != Version() {
		t.Fatalf("user_version %d (%v), want %d", v, err, Version())
	}
	// повторное открытие той же базы ничего не ломает
	closeAll()
	if _, err := Open(db.Dir); err != nil {
		t.Fatal(err)
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	db := openTemp(t)
	if _, err := db.Q().Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	closeAll()
	if _, err := Open(db.Dir); err == nil || !strings.Contains(err.Error(), "более новой") {
		t.Fatalf("want newer-schema error, got %v", err)
	}
}

type rec struct {
	N    int    `json:"n"`
	Name string `json:"name,omitempty"`
}

func TestTableEdit(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	tab := NewTable[*rec](TSubscriptions)
	if err := tab.Edit(ctx, db, func(m map[string]*rec) bool {
		m["a"], m["b"] = &rec{N: 1}, &rec{N: 2}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if err := tab.Edit(ctx, db, func(m map[string]*rec) bool {
		m["a"].N = 10
		delete(m, "b")
		return true
	}); err != nil {
		t.Fatal(err)
	}
	all, err := tab.All(ctx, db.Q())
	if err != nil || len(all) != 1 || all["a"].N != 10 {
		t.Fatalf("all = %v, %v", all, err)
	}
	if _, ok, _ := tab.Get(ctx, db.Q(), "b"); ok {
		t.Fatal("b must be deleted")
	}
}

// Edit атомарен: параллельные приращения не теряются (то, чего не давал
// «прочитать файл → записать файл» под мьютексом одного процесса).
func TestTableEditConcurrent(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	tab := NewTable[*rec](TSubscriptions)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := tab.Edit(ctx, db, func(m map[string]*rec) bool {
				if m["x"] == nil {
					m["x"] = &rec{}
				}
				m["x"].N++
				return true
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	r, _, err := tab.Get(ctx, db.Q(), "x")
	if err != nil || r.N != 20 {
		t.Fatalf("n = %v, %v", r, err)
	}
}

func TestKV(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	var v rec
	if ok, err := GetKV(ctx, db.Q(), KeyAccount, &v); ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := PutKV(ctx, db.Q(), KeyAccount, rec{N: 5}); err != nil {
		t.Fatal(err)
	}
	if ok, err := GetKV(ctx, db.Q(), KeyAccount, &v); !ok || err != nil || v.N != 5 {
		t.Fatal(ok, err, v)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, db *DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Q().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Установка до переезда: файлы переносятся при первом открытии и
// откладываются как .migrated.
func TestLegacyImport(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(closeAll)
	write(t, filepath.Join(dir, "outbox.json"), `[
	  {"id": "a", "chat": "saved", "text": "x", "status": "pending", "expires_at": "2026-11-12T10:00:00+00:00", "message_id": 9007199254740993},
	  {"id": "b", "chat": "saved", "text": "y", "status": "scheduled", "send_at": "2026-11-12T10:00:00+00:00", "future_field": {"k": 1}}
	]`)
	write(t, filepath.Join(dir, "feeds", "saved.ids"), "5\n7\n\x00\x00")
	write(t, filepath.Join(dir, "transcripts.json"), `{"c:1": "привет", "c:2": {"text": "т", "engine": "gigaam", "language": "ru"}}`)
	write(t, filepath.Join(dir, "subscriptions.json"), `{"s1": {"session": "x", "chats": {"saved": 5}}}`)
	write(t, filepath.Join(dir, "bot_offset.json"), `{"offset": 12, "bot": "77"}`)
	write(t, filepath.Join(dir, "chat_sets.json"), `{"updated": "2026-11-12T10:00:00Z", "sets": {"folder:1": {"members": []}}, "aliases": {"-100": "chan"}, "blocked": {"folder:1": [5, 6]}}`)
	write(t, filepath.Join(dir, "routes.json"), `{"saved": {"5": {"chain": ["s1"], "pos": 0, "at": "2026-11-12T10:00:00Z"}}}`)
	write(t, filepath.Join(dir, "peers.json"), `{"self": 42, "users": {"1": 11}, "channels": {"2": 22}, "usernames": {"bob": "user:1"}}`)
	write(t, filepath.Join(dir, "upstream.json"), `{"sources": {"web": {"values": {"v": "1"}}}}`)
	write(t, filepath.Join(dir, "agent_folders.json"), `[{"id": 15, "title": "T"}]`)

	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, "SELECT count(*) FROM drafts"); n != 2 {
		t.Fatalf("drafts %d", n)
	}
	var body string
	if err := db.Q().QueryRow("SELECT body FROM drafts WHERE id = 'a'").Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "9007199254740993") {
		t.Fatalf("большой id искажён: %s", body)
	}
	if err := db.Q().QueryRow("SELECT body FROM drafts WHERE id = 'b'").Scan(&body); err != nil || !strings.Contains(body, "future_field") {
		t.Fatalf("поле другой версии потеряно: %s %v", body, err)
	}
	if n := count(t, db, "SELECT count(*) FROM drafts WHERE status = 'scheduled' AND send_at > 0"); n != 1 {
		t.Fatalf("send_at not indexed: %d", n)
	}
	if n := count(t, db, "SELECT count(*) FROM feed WHERE chat = 'saved'"); n != 2 {
		t.Fatalf("feed %d", n)
	}
	if n := count(t, db, "SELECT count(*) FROM transcripts WHERE engine = 'telegram'"); n != 1 {
		t.Fatalf("transcripts %d", n)
	}
	if n := count(t, db, "SELECT count(*) FROM chat_set_blocked"); n != 2 {
		t.Fatalf("blocked %d", n)
	}
	if n := count(t, db, "SELECT count(*) FROM routes WHERE chat = 'saved' AND msg_id = 5 AND at > 0"); n != 1 {
		t.Fatalf("routes %d", n)
	}
	if n := count(t, db, "SELECT count(*) FROM peer_usernames"); n != 1 {
		t.Fatalf("usernames %d", n)
	}
	if n := count(t, db, "SELECT count(*) FROM agent_folders WHERE key = '15'"); n != 1 {
		t.Fatalf("agent folders %d", n)
	}
	var off struct {
		Offset int64  `json:"offset"`
		Bot    string `json:"bot"`
	}
	if ok, err := GetKV(context.Background(), db.Q(), KeyBotOffset, &off); !ok || err != nil || off.Offset != 12 {
		t.Fatal(ok, err, off)
	}
	for _, f := range []string{"outbox.json", "feeds", "transcripts.json", "subscriptions.json", "bot_offset.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s not moved: %v", f, err)
		}
		if _, err := os.Stat(filepath.Join(dir, f+".migrated")); err != nil {
			t.Errorf("%s.migrated: %v", f, err)
		}
	}
	audit, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if !strings.Contains(string(audit), "legacy_imported") {
		t.Fatalf("audit: %s", audit)
	}
}

// Старая сборка снова написала файл: в базу добавится только новое, база
// главнее.
func TestLegacyReappeared(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(closeAll)
	write(t, filepath.Join(dir, "subscriptions.json"), `{"s1": {"n": 1}}`)
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tab := NewTable[*rec](TSubscriptions)
	ctx := context.Background()
	if err := tab.Put(ctx, db.Q(), "s1", &rec{N: 100}); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "subscriptions.json"), `{"s1": {"n": 1}, "s2": {"n": 2}}`)
	if err := db.ImportLegacy(); err != nil {
		t.Fatal(err)
	}
	all, _ := tab.All(ctx, db.Q())
	if all["s1"].N != 100 || all["s2"] == nil || all["s2"].N != 2 {
		t.Fatalf("all = %+v", all)
	}
	audit, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if !strings.Contains(string(audit), "legacy_reappeared") {
		t.Fatalf("audit: %s", audit)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "subscriptions.json.migrated*"))
	if len(matches) != 2 {
		t.Fatalf("moved copies: %v", matches)
	}
}

// Испорченный файл откладывается и не мешает остальным.
func TestLegacyCorrupt(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(closeAll)
	write(t, filepath.Join(dir, "outbox.json"), `[{"id": "a"`)
	write(t, filepath.Join(dir, "pinned_drafts.json"), `{"a": 5}`)
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, "SELECT count(*) FROM pinned_drafts"); n != 1 {
		t.Fatalf("pins %d", n)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "outbox.json.broken-*"))
	if len(matches) != 1 {
		t.Fatalf("corrupt not quarantined: %v", matches)
	}
}

// Перенос не повторяется, если файл не удалось отложить (его держал другой
// процесс): строки не дублируются, аудит не шумит.
func TestLegacyMoveRetry(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(closeAll)
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pinned_drafts.json")
	write(t, path, `{"a": 5}`)
	st, _ := os.Stat(path)
	if err := db.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO legacy_imports(file, imported, size, mtime, rows) VALUES('pinned_drafts.json', 1, ?, ?, 1)",
			st.Size(), st.ModTime().UnixNano())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ImportLegacy(); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, "SELECT count(*) FROM pinned_drafts"); n != 0 {
		t.Fatalf("re-imported: %d", n)
	}
	if _, err := os.Stat(path + ".migrated"); err != nil {
		t.Fatal(err)
	}
}
