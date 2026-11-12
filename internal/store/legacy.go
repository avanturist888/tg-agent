package store

// Переезд установок с JSON-файлов (до 12.11.2026) на state.db.
//
// Open (и служба по расписанию — ImportLegacy) ищет в data/ старые файлы,
// переносит каждый в базу одной транзакцией, отмечает в legacy_imports и
// откладывает рядом как <файл>.migrated — не удаляет: откат на старую
// сборку — переименовать обратно. Так обновление у пользователя проходит
// само: служба после перезапуска открывает базу, и всё на месте.
//
// Перенос никогда не затирает базу: строки, которые в ней уже есть,
// остаются (INSERT OR IGNORE). Поэтому файл, появившийся снова (его
// записала ещё работающая старая сборка — например, открытое окно
// управления), безопасно влить ещё раз: добавится только новое
// (legacy_reappeared в аудите).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"tgagent/internal/audit"
	"tgagent/internal/fsx"
	"tgagent/internal/lock"
)

// Legacy — старый файл (или каталог) в data/ и как его перенести.
type Legacy struct {
	File string // путь относительно data/
	Dir  bool   // каталог (ленты)
	// Import переносит содержимое (path — полный путь) и возвращает,
	// сколько записей нашлось. Только INSERT OR IGNORE: база главнее.
	// ErrCorrupt (обёрнутая) — файл испорчен: его откладывают как .broken.
	// ErrRetry — перенести пока нельзя (например, не записался секрет):
	// файл остаётся на месте, попытка — при следующем ImportLegacy.
	Import func(ctx context.Context, tx *sql.Tx, path string) (int, error)
}

// ErrCorrupt — старый файл не разобрать.
var ErrCorrupt = errors.New("файл испорчен")

// ErrRetry — перенос отложен.
var ErrRetry = errors.New("перенос отложен")

var (
	legacyMu sync.Mutex
	legacy   []Legacy
)

// RegisterLegacy — перенос файла, формат которого знает другой пакет
// (например, где по пути нужен диспетчер учётных данных). Вызывать из init.
func RegisterLegacy(l Legacy) {
	legacyMu.Lock()
	defer legacyMu.Unlock()
	legacy = append(legacy, l)
}

func legacyList() []Legacy {
	legacyMu.Lock()
	defer legacyMu.Unlock()
	return append(slices.Clone(builtinLegacy), legacy...)
}

// ImportLegacy — перенести старые файлы, которые есть в data/. Ошибка —
// только у самой базы; испорченный файл откладывается и не мешает остальным.
func (db *DB) ImportLegacy() error {
	ctx := context.Background()
	for _, l := range legacyList() {
		if err := db.importOne(ctx, l); err != nil {
			return fmt.Errorf("%s: %w", l.File, err)
		}
	}
	return nil
}

func (db *DB) auditLog(action string, kv ...any) {
	_ = audit.Log(filepath.Join(db.Dir, "audit.jsonl"), action, kv...)
}

func (db *DB) importOne(ctx context.Context, l Legacy) error {
	path := filepath.Join(db.Dir, filepath.FromSlash(l.File))
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.IsDir() != l.Dir {
		return nil
	}
	size, mtime := st.Size(), st.ModTime().UnixNano()
	if l.Dir {
		size, mtime = dirStamp(path)
	}
	var (
		rows      int
		again     bool
		corrupt   error
		deferred  error
		skipWrite bool
	)
	err = db.Write(ctx, func(tx *sql.Tx) error {
		var pSize, pMtime int64
		e := tx.QueryRowContext(ctx, "SELECT size, mtime FROM legacy_imports WHERE file = ?", l.File).Scan(&pSize, &pMtime)
		switch {
		case e == nil:
			if pSize == size && pMtime == mtime {
				skipWrite = true // уже перенесён, не вышло только отложить файл
				return nil
			}
			again = true
		case !errors.Is(e, sql.ErrNoRows):
			return e
		}
		n, e := l.Import(ctx, tx, path)
		if errors.Is(e, ErrCorrupt) {
			corrupt = e
			return nil
		}
		if errors.Is(e, ErrRetry) {
			deferred = e
			return e // откатить всё, что успели
		}
		if e != nil {
			return e
		}
		rows = n
		_, e = tx.ExecContext(ctx, `INSERT INTO legacy_imports(file, imported, size, mtime, rows) VALUES(?, ?, ?, ?, ?)
			ON CONFLICT(file) DO UPDATE SET imported = excluded.imported, size = excluded.size, mtime = excluded.mtime, rows = excluded.rows`,
			l.File, time.Now().Unix(), size, mtime, n)
		return e
	})
	if deferred != nil {
		db.auditLog("legacy_import_deferred", "file", l.File, "error", deferred.Error())
		return nil
	}
	if err != nil {
		return err
	}
	if corrupt != nil {
		moved, qerr := fsx.Quarantine(path)
		db.auditLog("legacy_import_failed", "file", l.File, "error", corrupt.Error(), "moved_to", moved,
			"move_error", errString(qerr))
		return nil
	}
	moved := movedName(path)
	if err := os.Rename(path, moved); err != nil {
		// файл держит другой процесс — перенос уже в базе, отложим в следующий раз
		db.auditLog("legacy_move_failed", "file", l.File, "error", err.Error())
		return nil
	}
	_, _ = db.sql.ExecContext(ctx, "UPDATE legacy_imports SET moved_to = ? WHERE file = ?", filepath.Base(moved), l.File)
	if !l.Dir && !lock.Held(path) {
		_ = os.Remove(path + ".lock") // лок старого файла больше никому не нужен
	}
	if skipWrite {
		return nil
	}
	action := "legacy_imported"
	if again {
		action = "legacy_reappeared" // старая сборка ещё пишет файл
	}
	db.auditLog(action, "file", l.File, "rows", rows, "moved_to", filepath.Base(moved))
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// movedName — куда отложить перенесённый файл (прежний .migrated не трогаем).
func movedName(path string) string {
	p := path + ".migrated"
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return p
	}
	return path + ".migrated-" + time.Now().Format("20060102-150405")
}

// dirStamp — размер и время каталога по его файлам (у каталога на NTFS
// время меняется не от каждой дописанной строки).
func dirStamp(dir string) (int64, int64) {
	var size, mtime int64
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !info.IsDir() {
			size += info.Size()
			mtime = max(mtime, info.ModTime().UnixNano())
		}
	}
	return size, mtime
}

// readJSON — разобрать файл; пустой — как отсутствующий, из одних нулей
// (след сбоя питания) — испорчен.
func readJSON(path string, v any) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if fsx.Zeroed(raw) {
		return false, fmt.Errorf("%w: одни нули (след сбоя питания)", ErrCorrupt)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return false, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return true, nil
}

// insertDocs — перенос map[key]объект в таблицу «ключ + JSON».
func insertDocs(ctx context.Context, tx *sql.Tx, table, path string) (int, error) {
	var m map[string]json.RawMessage
	if ok, err := readJSON(path, &m); !ok {
		return 0, err
	}
	for k, v := range m {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO "+table+"(key, body) VALUES(?, ?)", k, compact(v)); err != nil {
			return 0, err
		}
	}
	return len(m), nil
}

// compact — JSON без отступов, байт в байт по значениям (не через any:
// id сообщений MAX больше 2⁵³ и во float64 исказились бы).
func compact(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return buf.String()
}

func insertKV(ctx context.Context, tx *sql.Tx, key string, raw json.RawMessage) error {
	_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO kv(key, value, updated) VALUES(?, ?, ?)", key, compact(raw), time.Now().Unix())
	return err
}

// kvFile — файл целиком становится одним значением kv.
func kvFile(file, key string) Legacy {
	return Legacy{File: file, Import: func(ctx context.Context, tx *sql.Tx, path string) (int, error) {
		var raw json.RawMessage
		if ok, err := readJSON(path, &raw); !ok {
			return 0, err
		}
		return 1, insertKV(ctx, tx, key, raw)
	}}
}

func docFile(file, table string) Legacy {
	return Legacy{File: file, Import: func(ctx context.Context, tx *sql.Tx, path string) (int, error) {
		return insertDocs(ctx, tx, table, path)
	}}
}

func unixOf(s string) int64 {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	return t.Unix()
}

var builtinLegacy = []Legacy{
	{File: "outbox.json", Import: importOutbox},
	{File: "feeds", Dir: true, Import: importFeeds},
	{File: "transcripts.json", Import: importTranscripts},
	{File: "peers.json", Import: importPeers},
	{File: "edit_dates.json", Import: importEditDates},
	{File: "updates_state.json", Import: importUpdates},
	{File: "routes.json", Import: importRoutes},
	{File: "chat_sets.json", Import: importSets},
	{File: "agent_folders.json", Import: importAgentFolders},
	docFile("subscriptions.json", TSubscriptions),
	docFile("access_requests.json", TAccessRequests),
	docFile("bot_ops.json", TBotOps),
	docFile("pinned_drafts.json", TPinnedDrafts),
	docFile("t3-threads.json", TT3Threads),
	docFile("upstream_cards.json", TUpstreamCards),
	docFile("account_notes.json", TAccountNotes),
	{File: "upstream.json", Import: importUpstream},
	kvFile("bot_offset.json", KeyBotOffset),
	kvFile("upstream_thread.json", KeyUpstreamThread),
	kvFile("account.json", KeyAccount),
	{File: "sending", Dir: true, Import: func(context.Context, *sql.Tx, string) (int, error) {
		return 0, nil // локи отправки до переезда: захват теперь в drafts
	}},
}

func importOutbox(ctx context.Context, tx *sql.Tx, path string) (int, error) {
	var drafts []json.RawMessage
	if ok, err := readJSON(path, &drafts); !ok {
		return 0, err
	}
	for _, raw := range drafts {
		var d struct {
			ID        string  `json:"id"`
			Status    string  `json:"status"`
			ExpiresAt string  `json:"expires_at"`
			SendAt    *string `json:"send_at"`
		}
		if err := json.Unmarshal(raw, &d); err != nil || d.ID == "" {
			continue
		}
		if d.Status == "" {
			d.Status = "pending"
		}
		var sendAt any
		if d.SendAt != nil {
			sendAt = unixOf(*d.SendAt)
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO drafts(id, status, expires, send_at, body) VALUES(?, ?, ?, ?, ?)",
			d.ID, d.Status, unixOf(d.ExpiresAt), sendAt, compact(raw)); err != nil {
			return 0, err
		}
	}
	return len(drafts), nil
}

func importFeeds(ctx context.Context, tx *sql.Tx, dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		alias, ok := strings.CutSuffix(e.Name(), ".ids")
		if !ok || e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return 0, err
		}
		for _, f := range strings.Fields(strings.ReplaceAll(string(raw), "\x00", " ")) {
			id, err := strconv.Atoi(f)
			if err != nil || id <= 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO feed(chat, msg_id) VALUES(?, ?)", alias, id); err != nil {
				return 0, err
			}
			n++
		}
	}
	return n, nil
}

func importTranscripts(ctx context.Context, tx *sql.Tx, path string) (int, error) {
	var m map[string]json.RawMessage
	if ok, err := readJSON(path, &m); !ok {
		return 0, err
	}
	for k, raw := range m {
		var v struct {
			Text     string `json:"text"`
			Engine   string `json:"engine"`
			Language string `json:"language"`
		}
		var text string
		if json.Unmarshal(raw, &text) == nil {
			v.Text, v.Engine = text, "telegram" // запись до движков — расшифровка Telegram
		} else if json.Unmarshal(raw, &v) != nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO transcripts(key, text, engine, language) VALUES(?, ?, ?, ?)",
			k, v.Text, v.Engine, v.Language); err != nil {
			return 0, err
		}
	}
	return len(m), nil
}

func importPeers(ctx context.Context, tx *sql.Tx, path string) (int, error) {
	var p struct {
		Self      int64             `json:"self"`
		Users     map[string]int64  `json:"users"`
		Channels  map[string]int64  `json:"channels"`
		Usernames map[string]string `json:"usernames"`
	}
	if ok, err := readJSON(path, &p); !ok {
		return 0, err
	}
	for table, m := range map[string]map[string]int64{"peer_users": p.Users, "peer_channels": p.Channels} {
		for k, h := range m {
			id, err := strconv.ParseInt(k, 10, 64)
			if err != nil {
				continue
			}
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO "+table+"(id, access_hash) VALUES(?, ?)", id, h); err != nil {
				return 0, err
			}
		}
	}
	for name, ref := range p.Usernames {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO peer_usernames(name, ref) VALUES(?, ?)", name, ref); err != nil {
			return 0, err
		}
	}
	if p.Self != 0 {
		if err := insertKV(ctx, tx, KeyPeersSelf, json.RawMessage(strconv.FormatInt(p.Self, 10))); err != nil {
			return 0, err
		}
	}
	return len(p.Users) + len(p.Channels) + len(p.Usernames), nil
}

func importEditDates(ctx context.Context, tx *sql.Tx, path string) (int, error) {
	var f struct {
		Alive int64          `json:"alive"`
		Seen  map[string]int `json:"seen"`
	}
	if ok, err := readJSON(path, &f); !ok {
		return 0, err
	}
	seen := f.Alive
	if seen == 0 {
		seen = time.Now().Unix()
	}
	for k, d := range f.Seen {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO edit_dates(key, edit_date, seen) VALUES(?, ?, ?)", k, d, seen); err != nil {
			return 0, err
		}
	}
	if f.Alive > 0 {
		if err := insertKV(ctx, tx, KeyEditsAlive, json.RawMessage(strconv.FormatInt(f.Alive, 10))); err != nil {
			return 0, err
		}
	}
	return len(f.Seen), nil
}

func importUpdates(ctx context.Context, tx *sql.Tx, path string) (int, error) {
	var f struct {
		User     int64            `json:"user"`
		State    json.RawMessage  `json:"state"`
		Channels map[string]int   `json:"channels"`
		Hashes   map[string]int64 `json:"access_hashes"`
	}
	if ok, err := readJSON(path, &f); !ok {
		return 0, err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM kv WHERE key = ?", KeyUpdatesState).Scan(&exists); err != nil {
		return 0, err
	}
	if exists > 0 {
		return 0, nil // отметка в базе новее: каналы без неё не сочетать
	}
	state := f.State
	if len(state) == 0 {
		state = json.RawMessage("null")
	}
	head, _ := json.Marshal(map[string]any{"user": f.User, "state": state})
	if err := insertKV(ctx, tx, KeyUpdatesState, head); err != nil {
		return 0, err
	}
	ids := map[int64]bool{}
	for k := range f.Channels {
		if id, err := strconv.ParseInt(k, 10, 64); err == nil {
			ids[id] = true
		}
	}
	for k := range f.Hashes {
		if id, err := strconv.ParseInt(k, 10, 64); err == nil {
			ids[id] = true
		}
	}
	for id := range ids {
		key := strconv.FormatInt(id, 10)
		var pts, hash any
		if v, ok := f.Channels[key]; ok {
			pts = v
		}
		if v, ok := f.Hashes[key]; ok {
			hash = v
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO update_channels(channel_id, pts, access_hash) VALUES(?, ?, ?)",
			id, pts, hash); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

func importRoutes(ctx context.Context, tx *sql.Tx, path string) (int, error) {
	var rs map[string]map[string]json.RawMessage
	if ok, err := readJSON(path, &rs); !ok {
		return 0, err
	}
	n := 0
	for chat, m := range rs {
		for k, raw := range m {
			id, err := strconv.Atoi(k)
			if err != nil {
				continue
			}
			var r struct {
				At time.Time `json:"at"`
			}
			_ = json.Unmarshal(raw, &r)
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO routes(chat, msg_id, at, body) VALUES(?, ?, ?, ?)",
				chat, id, r.At.Unix(), compact(raw)); err != nil {
				return 0, err
			}
			n++
		}
	}
	return n, nil
}

func importSets(ctx context.Context, tx *sql.Tx, path string) (int, error) {
	var st struct {
		Updated json.RawMessage            `json:"updated"`
		Sets    map[string]json.RawMessage `json:"sets"`
		Aliases map[string]string          `json:"aliases"`
		Blocked map[string][]int64         `json:"blocked"`
	}
	if ok, err := readJSON(path, &st); !ok {
		return 0, err
	}
	for k, v := range st.Sets {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO chat_sets(key, body) VALUES(?, ?)", k, compact(v)); err != nil {
			return 0, err
		}
	}
	for id, alias := range st.Aliases {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO chat_set_aliases(chat_id, alias) VALUES(?, ?)", id, alias); err != nil {
			return 0, err
		}
	}
	for key, ids := range st.Blocked {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO chat_set_blocked(set_key, chat_id) VALUES(?, ?)", key, id); err != nil {
				return 0, err
			}
		}
	}
	if len(st.Updated) > 0 {
		if err := insertKV(ctx, tx, KeySetsUpdated, st.Updated); err != nil {
			return 0, err
		}
	}
	return len(st.Sets), nil
}

func importAgentFolders(ctx context.Context, tx *sql.Tx, path string) (int, error) {
	var list []json.RawMessage
	if ok, err := readJSON(path, &list); !ok {
		return 0, err
	}
	for _, raw := range list {
		var f struct {
			ID int `json:"id"`
		}
		if json.Unmarshal(raw, &f) != nil || f.ID == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO agent_folders(key, body) VALUES(?, ?)",
			strconv.Itoa(f.ID), compact(raw)); err != nil {
			return 0, err
		}
	}
	return len(list), nil
}

func importUpstream(ctx context.Context, tx *sql.Tx, path string) (int, error) {
	var st struct {
		Sources map[string]json.RawMessage `json:"sources"`
	}
	if ok, err := readJSON(path, &st); !ok {
		return 0, err
	}
	for k, v := range st.Sources {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO upstream_sources(key, body) VALUES(?, ?)", k, compact(v)); err != nil {
			return 0, err
		}
	}
	return len(st.Sources), nil
}

// LegacyImported — сколько старых файлов уже перенесено (для tg doctor).
func (db *DB) LegacyImported() int {
	var n int
	_ = db.sql.QueryRowContext(context.Background(), "SELECT count(*) FROM legacy_imports").Scan(&n)
	return n
}
