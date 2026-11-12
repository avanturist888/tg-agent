package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tgagent/internal/lock"
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
	write(t, filepath.Join(dir, "upstream_cards.json"), `{"s1": {"id": 5, "text": "x"}}`)
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
	for _, f := range []string{"outbox.json", "feeds", "transcripts.json", "upstream_cards.json", "bot_offset.json"} {
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
	write(t, filepath.Join(dir, "upstream_cards.json"), `{"s1": {"n": 1}}`)
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tab := NewTable[*rec](TUpstreamCards)
	ctx := context.Background()
	if err := tab.Put(ctx, db.Q(), "s1", &rec{N: 100}); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "upstream_cards.json"), `{"s1": {"n": 1}, "s2": {"n": 2}}`)
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
	matches, _ := filepath.Glob(filepath.Join(dir, "upstream_cards.json.migrated*"))
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

// Старая сборка держит лок файла (пишет очередь): перенос не читает файл
// под ней, а откладывается и проходит, как только лок свободен.
func TestLegacyWaitsOldWriter(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(closeAll)
	path := filepath.Join(dir, "outbox.json")
	write(t, path, `[{"id": "a", "status": "pending"}]`)
	old := lock.New(path, 0)
	if err := old.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !db.LegacyPending() || count(t, db, "SELECT count(*) FROM drafts") != 0 {
		t.Fatal("перенос не отложен, пока старая сборка держит файл")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("файл старой сборки тронут:", err)
	}
	old.Release()
	if err := db.ImportLegacy(); err != nil {
		t.Fatal(err)
	}
	if db.LegacyPending() || count(t, db, "SELECT count(*) FROM drafts") != 1 {
		t.Fatal("после лока не перенесено")
	}
}

// Старая сборка ещё отправляет черновик (её лок в data/sending жив): очередь
// не переносится — иначе новая служба отправила бы его второй раз.
func TestLegacyWaitsOldSender(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(closeAll)
	write(t, filepath.Join(dir, "outbox.json"), `[{"id": "a", "status": "approved"}]`)
	sending := filepath.Join(dir, "sending")
	os.MkdirAll(sending, 0o700)
	sender := lock.New(filepath.Join(sending, "a"), 0)
	if err := sender.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if count(t, db, "SELECT count(*) FROM drafts") != 0 || !db.LegacyPending() {
		t.Fatal("очередь перенесена при живой старой отправке")
	}
	// старая сборка отправила и записала итог
	write(t, filepath.Join(dir, "outbox.json"), `[{"id": "a", "status": "sent", "message_id": 5}]`)
	sender.Release()
	if err := db.ImportLegacy(); err != nil {
		t.Fatal(err)
	}
	if count(t, db, "SELECT count(*) FROM drafts WHERE id = 'a' AND status = 'sent'") != 1 {
		t.Fatal("итог старой отправки не перенесён")
	}
	if _, err := os.Stat(sending); !os.IsNotExist(err) {
		t.Fatal("каталог старых локов не отложен")
	}
}

// Файл снова записала старая сборка, и черновик в нём уже решён: итог
// главнее нерешённого в базе (иначе — повторная отправка или отправка
// отменённого); нерешённое в файле базу не откатывает.
func TestLegacyFinalWins(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(closeAll)
	write(t, filepath.Join(dir, "outbox.json"), `[{"id": "a", "status": "scheduled", "send_at": "2099-01-01T00:00:00+00:00"},
		{"id": "b", "status": "pending"}, {"id": "c", "status": "sent"}]`)
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "outbox.json"), `[{"id": "a", "status": "cancelled"},
		{"id": "b", "status": "pending", "text": "старое"}, {"id": "c", "status": "pending"}]`)
	if err := db.ImportLegacy(); err != nil {
		t.Fatal(err)
	}
	if count(t, db, "SELECT count(*) FROM drafts WHERE id = 'a' AND status = 'cancelled' AND send_at IS NULL") != 1 {
		t.Fatal("отмена старой сборки не перенесена")
	}
	if count(t, db, "SELECT count(*) FROM drafts WHERE id = 'c' AND status = 'sent'") != 1 {
		t.Fatal("решённое в базе откатилось")
	}
}

// Повторный перенос (откат и снова обновление): старая сборка взяла в
// отправку черновик, который база знает нерешённым, и итога не записала.
// Он — попытка («могло уйти»): ни автоотправка, ни досылка его не повторят.
func TestLegacyReimportApprovedIsAttempt(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(closeAll)
	write(t, filepath.Join(dir, "outbox.json"), `[{"id": "a", "status": "scheduled", "send_at": "2099-01-01T00:00:00+00:00"},
		{"id": "b", "status": "pending"}]`)
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Q().Exec("UPDATE drafts SET status = 'approved' WHERE id = 'b'"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "outbox.json"), `[{"id": "a", "status": "approved"}, {"id": "b", "status": "approved"}]`)
	if err := db.ImportLegacy(); err != nil {
		t.Fatal(err)
	}
	if count(t, db, "SELECT count(*) FROM drafts WHERE id = 'a' AND status = 'approved' AND send_at IS NULL AND send_attempts = 1") != 1 {
		t.Fatal("взятое старой сборкой из автоотправки — не попытка")
	}
	if count(t, db, "SELECT send_attempts FROM drafts WHERE id = 'b'") != 1 {
		t.Fatal("одобренное без отметки попытки — не попытка")
	}
}

// Два процесса открывают базу одновременно: перенос — один раз, оба без ошибки.
func TestLegacyConcurrentOpen(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(closeAll)
	write(t, filepath.Join(dir, "pinned_drafts.json"), `{"a": 5}`)
	write(t, filepath.Join(dir, "outbox.json"), `[{"id": "a", "status": "pending"}]`)
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// «второй процесс» — та же процедура параллельно
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := db.ImportLegacy(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if count(t, db, "SELECT count(*) FROM drafts") != 1 || count(t, db, "SELECT count(*) FROM pinned_drafts") != 1 {
		t.Fatal("перенос задвоен или потерян")
	}
}

// Пока tg cutover не отметил, что процессов старой сборки нет, старые
// файлы не трогаются (живой старый процесс писал бы мимо базы); после
// отметки — переносятся.
func TestLegacyWaitsCutover(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(closeAll)
	RequireCutover = true
	t.Cleanup(func() { RequireCutover = false })
	write(t, filepath.Join(dir, "upstream_cards.json"), `{"s1": {"n": 1}}`)
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, "SELECT count(*) FROM upstream_cards"); n != 0 {
		t.Fatalf("перенесено до отметки: %d", n)
	}
	if !db.LegacyPending() || db.LegacyBlocked() == "" {
		t.Fatal("перенос не помечен как ждущий")
	}
	if _, err := os.Stat(filepath.Join(dir, "upstream_cards.json")); err != nil {
		t.Fatal("старый файл тронут до отметки")
	}
	if got := LegacyPresent(dir); len(got) != 1 || got[0] != "upstream_cards.json" {
		t.Fatalf("LegacyPresent = %v", got)
	}
	if err := MarkCutover(dir, "test"); err != nil {
		t.Fatal(err)
	}
	if err := db.ImportLegacy(); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, "SELECT count(*) FROM upstream_cards"); n != 1 || db.LegacyPending() || db.LegacyBlocked() != "" {
		t.Fatalf("после отметки: строк %d, pending %v, blocked %q", n, db.LegacyPending(), db.LegacyBlocked())
	}
	audit, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if strings.Count(string(audit), "legacy_import_waiting") != 1 {
		t.Fatalf("audit: %s", audit)
	}
	// всё перенесено — отметка снята: старый файл, вернувшийся потом (откат),
	// снова ждёт нового tg cutover
	if _, err := os.Stat(filepath.Join(dir, CutoverFile)); !os.IsNotExist(err) {
		t.Fatal("отметка пережила перенос")
	}
	write(t, filepath.Join(dir, "upstream_cards.json"), `{"s2": {"n": 2}}`)
	if err := db.ImportLegacy(); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, "SELECT count(*) FROM upstream_cards"); n != 1 || db.LegacyBlocked() == "" {
		t.Fatalf("вернувшийся файл перенесён без нового cutover: строк %d", n)
	}
	// снятие отметки до замены exe (build.ps1)
	if err := MarkCutover(dir, "test"); err != nil {
		t.Fatal(err)
	}
	if err := RevokeCutover(dir); err != nil || RevokeCutover(dir) != nil {
		t.Fatalf("RevokeCutover: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, CutoverFile)); !os.IsNotExist(err) {
		t.Fatal("отметка не снята")
	}
}

// Заметки к аккаунту в снова появившемся файле сливаются по id, а не
// теряются из-за того, что у аккаунта в базе уже есть заметки.
func TestLegacyNotesMerge(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(closeAll)
	write(t, filepath.Join(dir, "account_notes.json"), `{"main": [{"id": "n1", "text": "раз"}]}`)
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "account_notes.json"), `{"main": [{"id": "n1", "text": "раз (старая правка)"}, {"id": "n2", "text": "два"}], "work": [{"id": "n3", "text": "три"}]}`)
	if err := db.ImportLegacy(); err != nil {
		t.Fatal(err)
	}
	var main, work string
	if err := db.Q().QueryRow("SELECT body FROM account_notes WHERE key = 'main'").Scan(&main); err != nil {
		t.Fatal(err)
	}
	if err := db.Q().QueryRow("SELECT body FROM account_notes WHERE key = 'work'").Scan(&work); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(main, `"n2"`) || strings.Contains(main, "старая правка") || strings.Count(main, `"id"`) != 2 || !strings.Contains(work, `"n3"`) {
		t.Fatalf("main = %s, work = %s", main, work)
	}
}

// База схемы 1 с захватом, чей процесс умер (мог уже отправить): после
// перехода на схему 2 это попытка, а не первая отправка.
func TestMigrateKeepsClaimAsAttempt(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(closeAll)
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, FileName)))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{migrations[0],
		`INSERT INTO drafts(id, status, expires, sender_pid, sender_since, body) VALUES('a', 'approved', 0, 999999, 1, '{"id":"a"}')`,
		`INSERT INTO drafts(id, status, expires, body) VALUES('b', 'approved', 0, '{"id":"b"}')`,
		`PRAGMA user_version = 1`} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, "SELECT send_attempts FROM drafts WHERE id = 'a'"); n != 1 {
		t.Fatalf("захват схемы 1: попыток %d", n)
	}
	if n := count(t, db, "SELECT send_attempts FROM drafts WHERE id = 'b'"); n != 0 {
		t.Fatalf("без захвата: попыток %d", n)
	}
}

// Старая сборка не отмечала начало отправки: approved в её файле — уже
// взятое в отправку, с локом отправки или без. Такой черновик — попытка
// («могло уйти»), ждущий решения — нет.
func TestLegacyApprovedIsAttempt(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(closeAll)
	write(t, filepath.Join(dir, "outbox.json"), `[{"id": "a", "status": "approved", "expires_at": "2099-01-01T00:00:00Z"},
		{"id": "b", "status": "approved", "expires_at": "2099-01-01T00:00:00Z"},
		{"id": "c", "status": "pending", "expires_at": "2099-01-01T00:00:00Z"}]`)
	write(t, filepath.Join(dir, "sending", "a.lock"), `{"pid": 999999, "ts": 1}`)
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	q := func(id string) int { return count(t, db, "SELECT send_attempts FROM drafts WHERE id = '"+id+"'") }
	if a, b, c := q("a"), q("b"), q("c"); a != 1 || b != 1 || c != 0 {
		t.Fatalf("попытки: a %d, b %d, c %d", a, b, c)
	}
}

// Лок отправки без владельца (упал между созданием файла и записью pid),
// давний: перенос не ждёт его вечно, а черновик переносится попыткой.
func TestLegacyMalformedSendLock(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(closeAll)
	write(t, filepath.Join(dir, "outbox.json"), `[{"id": "a", "status": "approved", "expires_at": "2099-01-01T00:00:00Z"}]`)
	lockFile := filepath.Join(dir, "sending", "a.lock")
	write(t, lockFile, "")
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(lockFile, old, old); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ImportLegacy(); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, "SELECT send_attempts FROM drafts WHERE id = 'a'"); n != 1 || db.LegacyPending() {
		t.Fatalf("попыток %d, pending %v", n, db.LegacyPending())
	}
}

// Выгрузка для отката: очередь — из базы (не из устаревшего снимка), попытка
// без итога — со сбоем (старая сборка её не дошлёт), ушедшее без отметки —
// отправленным; прочие снимки — на место; после выгрузки перенос ждёт.
func TestExportLegacy(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(closeAll)
	write(t, filepath.Join(dir, "outbox.json"), `[
		{"id": "a", "status": "scheduled", "send_at": "2099-01-01T00:00:00+00:00"},
		{"id": "b", "status": "pending"}, {"id": "c", "status": "pending"},
		{"id": "d", "status": "pending"}, {"id": "e", "status": "pending"},
		{"id": "f", "status": "pending", "reply_to": 9007199254740993}]`)
	write(t, filepath.Join(dir, "pinned_drafts.json"), `{"a": 5}`)
	write(t, filepath.Join(dir, "transcripts.json"), `{}`)
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"UPDATE drafts SET status = 'sent', body = json_set(body, '$.status', 'sent', '$.message_id', 9) WHERE id = 'a'",
		"UPDATE drafts SET status = 'approved', send_attempts = 1 WHERE id = 'b'",
		"UPDATE drafts SET status = 'approved' WHERE id = 'c'",
		"UPDATE drafts SET status = 'approved', send_attempts = 1 WHERE id = 'd'",
		"UPDATE drafts SET status = 'cancelled' WHERE id = 'e'",
		// закреплена после переноса — в снимке её нет
		"INSERT INTO pinned_drafts(key, body) VALUES('b', '7')",
	} {
		if _, err := db.Q().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(dir, UnsavedDir, "d"), `{"message_id": 77, "at": "2026-11-12T10:00:00Z"}`)
	restored, err := ExportLegacy(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(restored, "transcripts.json") || !slices.Contains(restored, "pinned_drafts.json (из базы)") {
		t.Fatalf("вернулось: %v", restored)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "pinned_drafts.json")); err != nil || !strings.Contains(string(raw), `"b": 7`) {
		t.Fatalf("закреплённое после переноса не выгружено: %v %s", err, raw)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "outbox.json")); err != nil || !strings.Contains(string(raw), "9007199254740993") {
		t.Fatalf("большой id искажён: %v", err)
	}
	var got []map[string]any
	raw, err := os.ReadFile(filepath.Join(dir, "outbox.json"))
	if err != nil || json.Unmarshal(raw, &got) != nil {
		t.Fatalf("outbox.json: %v", err)
	}
	by := map[string]map[string]any{}
	for _, d := range got {
		by[d["id"].(string)] = d
	}
	if by["a"]["status"] != "sent" || by["e"]["status"] != "cancelled" {
		t.Fatalf("решённые из базы: a %v, e %v", by["a"]["status"], by["e"]["status"])
	}
	if by["b"]["status"] != "approved" || by["b"]["send_error"] == nil {
		t.Fatalf("попытка без итога: %v", by["b"])
	}
	if by["c"]["status"] != "approved" || by["c"]["send_error"] != nil {
		t.Fatalf("одобренное без попытки: %v", by["c"])
	}
	if by["d"]["status"] != "sent" || by["d"]["message_id"] != float64(77) {
		t.Fatalf("ушедшее без отметки: %v", by["d"])
	}
	if !RolledBack(dir) {
		t.Fatal("нет отметки отката")
	}
	if err := db.ImportLegacy(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "outbox.json")); err != nil || db.LegacyBlocked() == "" {
		t.Fatalf("после отката перенос не ждёт: %v %q", err, db.LegacyBlocked())
	}
	// прерванную выгрузку можно повторить: свои файлы она перезапишет —
	// и тогда, когда процесс умер, не записав размер и время файла
	if _, err := db.Q().Exec("UPDATE legacy_imports SET size = 0, mtime = 0 WHERE exported != 0"); err != nil {
		t.Fatal(err)
	}
	if _, err := ExportLegacy(dir); err != nil {
		t.Fatalf("повтор выгрузки: %v", err)
	}
	// а файл, который уже вела старая сборка, — нет
	write(t, filepath.Join(dir, "outbox.json"), `[{"id": "c", "status": "pending", "text": "исправлено"}]`)
	if _, err := ExportLegacy(dir); err == nil {
		t.Fatal("выгрузка поверх файла старой сборки")
	}
}

// Повторное обновление после отката: выгруженные файлы, которые потом вела
// старая сборка, главнее базы — исправленный текст ждущего черновика, снятый
// блок группы. Одобренное ею без итога — попытка.
func TestReimportAfterRollback(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(closeAll)
	write(t, filepath.Join(dir, "outbox.json"), `[{"id": "a", "status": "pending", "text": "старое"},
		{"id": "b", "status": "scheduled", "send_at": "2099-01-01T00:00:00+00:00"}]`)
	write(t, filepath.Join(dir, "chat_sets.json"), `{"sets": {}, "aliases": {}, "blocked": {"f1": [5, 6]}}`)
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExportLegacy(dir); err != nil {
		t.Fatal(err)
	}
	// старая сборка: исправила текст, одобрила b (и упала до итога), сняла блок 6
	write(t, filepath.Join(dir, "outbox.json"), `[{"id": "a", "status": "pending", "text": "исправлено"},
		{"id": "b", "status": "approved"}]`)
	write(t, filepath.Join(dir, "chat_sets.json"), `{"sets": {}, "aliases": {}, "blocked": {"f1": [5]}}`)
	if err := ClearRollback(dir); err != nil { // tg cutover
		t.Fatal(err)
	}
	if err := db.ImportLegacy(); err != nil {
		t.Fatal(err)
	}
	if count(t, db, "SELECT count(*) FROM drafts WHERE id = 'a' AND json_extract(body, '$.text') = 'исправлено'") != 1 {
		t.Fatal("исправленный старой сборкой текст потерян")
	}
	if count(t, db, "SELECT count(*) FROM drafts WHERE id = 'b' AND status = 'approved' AND send_attempts = 1 AND send_at IS NULL") != 1 {
		t.Fatal("одобренное старой сборкой — не попытка")
	}
	if count(t, db, "SELECT count(*) FROM chat_set_blocked") != 1 {
		t.Fatal("группы чатов не взяты из файла")
	}
	if count(t, db, "SELECT count(*) FROM legacy_imports WHERE exported != 0") != 0 {
		t.Fatal("отметка выгрузки не снята после переноса")
	}
}

// Паника внутри транзакции (её ловят выше) не оставляет базу запертой для
// записи.
func TestWritePanicReleases(t *testing.T) {
	t.Cleanup(closeAll)
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() { _ = recover() }()
		_ = db.Write(context.Background(), func(tx *sql.Tx) error {
			if _, err := tx.Exec("INSERT INTO kv(key, value, updated) VALUES('a', '1', 0)"); err != nil {
				t.Fatal(err)
			}
			panic("сбой")
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO kv(key, value, updated) VALUES('b', '2', 0)")
		return err
	}); err != nil {
		t.Fatalf("после паники запись не идёт: %v", err)
	}
	if count(t, db, "SELECT count(*) FROM kv WHERE key = 'a'") != 0 {
		t.Fatal("запись паникнувшей транзакции осталась")
	}
}

// Испорченный выгруженный файл при повторном переносе базу не трогает:
// Replace очищает таблицы в той же транзакции, и сбой откатывает очистку.
func TestReimportCorruptKeepsDB(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(closeAll)
	write(t, filepath.Join(dir, "chat_sets.json"), `{"sets": {}, "aliases": {}, "blocked": {"f1": [5, 6]}}`)
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExportLegacy(dir); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "chat_sets.json"), `{"sets": `)
	if err := ClearRollback(dir); err != nil {
		t.Fatal(err)
	}
	if err := db.ImportLegacy(); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, "SELECT count(*) FROM chat_set_blocked"); n != 2 {
		t.Fatalf("блоки после испорченного файла: %d", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "chat_sets.json")); !os.IsNotExist(err) {
		t.Fatal("испорченный файл не отложен")
	}
}

// После выгрузки (tg rollback) запись в базу закрыта, после tg cutover —
// снова открыта.
func TestWriteRefusedAfterRollback(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(closeAll)
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExportLegacy(dir); err != nil {
		t.Fatal(err)
	}
	put := func() error {
		return db.Write(context.Background(), func(tx *sql.Tx) error {
			return PutKV(context.Background(), tx, "x", 1)
		})
	}
	if err := put(); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("запись после отката: %v", err)
	}
	if err := ClearRollback(dir); err != nil {
		t.Fatal(err)
	}
	if err := put(); err != nil {
		t.Fatalf("запись после cutover: %v", err)
	}
}

// Пустой выгруженный файл при повторном переносе — испорченный: таблицы базы
// по нему не очищаются.
func TestReimportEmptyKeepsDB(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(closeAll)
	write(t, filepath.Join(dir, "chat_sets.json"), `{"sets": {}, "aliases": {}, "blocked": {"f1": [5, 6]}}`)
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExportLegacy(dir); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "chat_sets.json"), "  \n")
	if err := ClearRollback(dir); err != nil {
		t.Fatal(err)
	}
	if err := db.ImportLegacy(); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, "SELECT count(*) FROM chat_set_blocked"); n != 2 {
		t.Fatalf("блоки после пустого файла: %d", n)
	}
}

// Одобренное без попытки выгрузка отдаёт старой сборке на досылку и в базе
// делает попыткой: старая сборка могла отправить и умереть до отметки, не
// тронув outbox.json, — повторный перенос пропустит файл, а база не должна
// снова отдать черновик в отправку.
func TestExportHandsApprovedOnce(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(closeAll)
	write(t, filepath.Join(dir, "outbox.json"), `[{"id": "c", "status": "pending"}]`)
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Q().Exec("UPDATE drafts SET status = 'approved' WHERE id = 'c'"); err != nil {
		t.Fatal(err)
	}
	if _, err := ExportLegacy(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "outbox.json"))
	if err != nil || !strings.Contains(string(raw), `"approved"`) || strings.Contains(string(raw), "send_error") {
		t.Fatalf("старой сборке — не на досылку: %v %s", err, raw)
	}
	if err := ClearRollback(dir); err != nil { // файл не тронут
		t.Fatal(err)
	}
	if err := db.ImportLegacy(); err != nil {
		t.Fatal(err)
	}
	if count(t, db, "SELECT count(*) FROM drafts WHERE id = 'c' AND status = 'approved' AND send_attempts = 1") != 1 {
		t.Fatal("после повторного переноса черновик снова отдаётся в отправку")
	}
}
