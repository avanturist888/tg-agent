package store

// Откат на сборку до state.db (tg rollback).
//
// Отложенные *.migrated — снимки на момент переноса: всё, что было потом,
// есть только в базе. Поэтому файл старой сборки собирается из базы
// (Legacy.Export / ExportDir), а снимок возвращается только у кэшей, чей
// устаревший снимок ничем не грозит (Legacy.Snapshot). Что из двух — каждый
// старый файл объявляет сам (проверяет TestLegacyRollbackDeclared).
//
// Попытку отправки без итога старая сборка различать не умеет: approved без
// send_error она дослала бы при старте. Такой черновик выгружается со
// сбоем «могло уйти» — она его не тронет, повтор — только явный. Ушедшее, чья
// отметка не легла в базу (sent-unsaved), выгружается отправленным.
//
// Выгрузку можно повторить. До записи файла в legacy_imports ложится
// намерение — хеш его содержимого (export_hash): повтор узнаёт свой файл по
// нему (не тронут — перезаписать можно), а чужой (его уже вела старая
// сборка) не трогает. Файл пишется целиком или никак (временный файл и
// переименование; каталог — так же).
//
// После выгрузки (отметка RollbackFile) эта сборка не берёт черновики в
// отправку и не переносит старые файлы: иначе отправленное ею после
// выгрузки старая сборка не увидела бы. Снимает отметку обновление —
// tg cutover (ClearRollback). Тогда выгруженный файл, который вела старая
// сборка, главнее базы (Legacy.Replace).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"tgagent/internal/fsx"
	"tgagent/internal/lock"
)

// RollbackFile — отметка в data/ профиля: состояние выгружено для старой
// сборки (tg rollback).
const RollbackFile = "legacy-rollback"

// RolledBack — выгружено ли состояние data/ dir для старой сборки.
func RolledBack(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, RollbackFile))
	return err == nil
}

// ClearRollback — снять отметку отката (tg cutover: процессов старой
// сборки больше нет, её файлы снова переносятся в базу).
func ClearRollback(dir string) error {
	err := os.Remove(filepath.Join(dir, RollbackFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// UnsavedDir — квитанции ушедшего, чья отметка не легла в базу
// (core.markSentDurably): <data>/sent-unsaved/<id>, внутри message_id и at.
const UnsavedDir = "sent-unsaved"

// uncertainSend — сбой для попытки без итога (тот же смысл, что у deliver).
const uncertainSend = "прошлая попытка отправки прервалась без итога — сообщение могло уйти частью или целиком. " +
	"Проверь чат; повторить — tg_send_draft"

// export — что выгрузка кладёт на место одного старого файла.
type export struct {
	l     Legacy
	file  []byte            // файл
	files map[string][]byte // каталог: имя → содержимое
	hash  string
}

func (e *export) seal() {
	h := sha256.New()
	if e.l.Dir {
		names := make([]string, 0, len(e.files))
		for n := range e.files {
			names = append(names, n)
		}
		slices.Sort(names)
		for _, n := range names {
			h.Write([]byte(n))
			h.Write([]byte{0})
			h.Write(e.files[n])
			h.Write([]byte{0})
		}
	} else {
		h.Write(e.file)
	}
	e.hash = hex.EncodeToString(h.Sum(nil))
}

// ExportLegacy — выгрузить состояние data/ dir для старой сборки (см. в
// начале файла). Процессы установки должны быть остановлены (tg rollback
// это проверяет); живой отправитель черновика — отказ. Сбой возврата одного
// снимка не мешает остальным. Возвращает, что легло.
func ExportLegacy(dir string) ([]string, error) {
	db, err := Open(dir)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	list := legacyList()
	for _, l := range list {
		if l.Export == nil && l.ExportDir == nil {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(l.File))
		if !ownExport(ctx, db, l, p) {
			return nil, fmt.Errorf("%s уже лежит, и выгружала его не эта выгрузка (ещё не перенесён или его вела старая сборка) — поверх него нельзя", p)
		}
	}
	if err := RevokeCutover(dir); err != nil {
		return nil, err
	}
	var exps []*export
	// одна транзакция записи: отметка отката — под ней (запись других
	// процессов, начатая после, видит отметку и отказывает, см. DB.Write),
	// никто не поменяет базу, пока её читаем, и намерения (хеши) ложатся до
	// записи файлов
	err = db.write(ctx, true, func(tx *sql.Tx) error {
		raw, _ := json.Marshal(map[string]any{"at": time.Now().Format(time.RFC3339)})
		if err := fsx.WriteFile(filepath.Join(dir, RollbackFile), raw, 0o600); err != nil {
			return err
		}
		exps = nil
		for _, l := range list {
			e := &export{l: l}
			var (
				err   error
				empty bool
			)
			switch {
			case l.ExportDir != nil:
				e.files, err = l.ExportDir(ctx, tx, dir)
				empty = e.files == nil
			case l.Export != nil:
				e.file, err = l.Export(ctx, tx, dir)
				empty = e.file == nil
			default:
				continue
			}
			if err != nil {
				return fmt.Errorf("%s: %w", l.File, err)
			}
			if empty {
				continue // в базе нечего отдавать
			}
			e.seal()
			if _, err := tx.ExecContext(ctx, `INSERT INTO legacy_imports(file, imported, size, mtime, rows, exported, export_hash) VALUES(?, ?, 0, 0, 0, 1, ?)
				ON CONFLICT(file) DO UPDATE SET exported = 1, export_hash = excluded.export_hash`,
				l.File, time.Now().Unix(), e.hash); err != nil {
				return err
			}
			exps = append(exps, e)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var done []string
	for _, e := range exps {
		p := filepath.Join(dir, filepath.FromSlash(e.l.File))
		if err := e.write(p); err != nil {
			return done, fmt.Errorf("%s: %w", e.l.File, err)
		}
		// размер и время — чтобы перенос узнал нетронутый файл и не вливал его
		size, mtime, err := stampOf(p, e.l.Dir)
		if err == nil {
			_, err = db.Q().ExecContext(ctx, "UPDATE legacy_imports SET size = ?, mtime = ? WHERE file = ?", size, mtime, e.l.File)
		}
		if err != nil {
			return done, fmt.Errorf("%s: %w", e.l.File, err)
		}
		done = append(done, e.l.File+" (из базы)")
	}
	moves, err := snapshots(ctx, db, list)
	if err != nil {
		return done, err
	}
	var errs []error
	for _, m := range moves {
		dst := filepath.Join(dir, filepath.FromSlash(m.file))
		src := filepath.Join(filepath.Dir(dst), m.from)
		if _, err := os.Stat(dst); err == nil {
			continue // уже на месте (прошлая выгрузка) или старая сборка завела новый
		}
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			errs = append(errs, fmt.Errorf("%s не вернуть на место (повтори tg rollback): %w", m.file, err))
			continue
		}
		done = append(done, m.file)
	}
	return done, errors.Join(errs...)
}

// write — положить выгрузку на место целиком: файл — fsx.WriteFile,
// каталог — собрать рядом и переименовать (прежний свой — заменить).
func (e *export) write(p string) error {
	if !e.l.Dir {
		return fsx.WriteFile(p, e.file, 0o600)
	}
	tmp := p + ".export-tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	for n, b := range e.files {
		if err := fsx.WriteFile(filepath.Join(tmp, n), b, 0o600); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(p); err != nil { // свой прошлый (ownExport это проверил)
		return err
	}
	return os.Rename(tmp, p)
}

// ownExport — файла (каталога) нет или его выгрузила выгрузка, и он не
// тронут после: содержимое совпадает с записанным намерением.
func ownExport(ctx context.Context, db *DB, l Legacy, path string) bool {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return true
	}
	var exp int64
	var want string
	if err := db.Q().QueryRowContext(ctx, "SELECT exported, export_hash FROM legacy_imports WHERE file = ?", l.File).Scan(&exp, &want); err != nil || exp == 0 {
		return false
	}
	e := &export{l: l}
	if l.Dir {
		entries, err := os.ReadDir(path)
		if err != nil {
			return false
		}
		e.files = map[string][]byte{}
		for _, en := range entries {
			b, err := os.ReadFile(filepath.Join(path, en.Name()))
			if err != nil {
				return false
			}
			e.files[en.Name()] = b
		}
	} else {
		b, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		e.file = b
	}
	e.seal()
	return e.hash == want
}

func stampOf(p string, dir bool) (int64, int64, error) {
	if dir {
		size, mtime := dirStamp(p)
		return size, mtime, nil
	}
	st, err := os.Stat(p)
	if err != nil {
		return 0, 0, err
	}
	return st.Size(), st.ModTime().UnixNano(), nil
}

type snapshot struct{ file, from string }

// snapshots — перенесённые файлы, которые возвращаются снимком.
func snapshots(ctx context.Context, db *DB, list []Legacy) ([]snapshot, error) {
	rows, err := db.Q().QueryContext(ctx, "SELECT file, moved_to FROM legacy_imports WHERE moved_to != ''")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []snapshot
	for rows.Next() {
		var m snapshot
		if err := rows.Scan(&m.file, &m.from); err != nil {
			return nil, err
		}
		if slices.ContainsFunc(list, func(l Legacy) bool { return l.File == m.file && l.Snapshot }) {
			out = append(out, m)
		}
	}
	return out, rows.Err()
}

func jsonOut(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

// exportOutbox — очередь в формате старой сборки. Попытка без итога — со
// сбоем «могло уйти» (старая сборка дослала бы approved без send_error),
// ушедшее без отметки (sent-unsaved) — отправленным. Тело — как есть, без
// разбора чисел (id сообщений MAX больше 2⁵³ в float64 исказились бы).
func exportOutbox(ctx context.Context, tx *sql.Tx, dir string) ([]byte, error) {
	drafts := []map[string]json.RawMessage{}
	err := eachRow(ctx, tx, "SELECT id, status, body, send_attempts, sender_pid, sender_since FROM drafts ORDER BY seq", func(r *sql.Rows) error {
		var (
			id, status, body string
			attempts         int
			pid              sql.NullInt64
			since            sql.NullFloat64
		)
		if err := r.Scan(&id, &status, &body, &attempts, &pid, &since); err != nil {
			return err
		}
		if pid.Valid && lock.PIDAlive(int(pid.Int64), since.Float64) {
			return fmt.Errorf("черновик %s сейчас отправляет процесс %d — останови его и повтори", id, pid.Int64)
		}
		m := map[string]json.RawMessage{}
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			return fmt.Errorf("черновик %s: %w", id, err)
		}
		m["status"] = jsonString(status)
		noError := len(m["send_error"]) == 0 || string(m["send_error"]) == "null"
		if mid, at, ok := unsavedReceipt(dir, id); ok && status != "sent" {
			// ушло, отметка не легла (могло и протухнуть, пока ждали)
			m["status"], m["message_id"], m["sent_at"] = jsonString("sent"), json.RawMessage(strconv.FormatInt(mid, 10)), jsonString(at)
			delete(m, "send_error")
		} else if status == "approved" && attempts > 0 && noError {
			m["send_error"] = jsonString(uncertainSend)
		}
		drafts = append(drafts, m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return jsonOut(drafts)
}

func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// exportFeeds — ленты: <alias>.ids с id по строке (importFeeds наоборот).
func exportFeeds(ctx context.Context, tx *sql.Tx, _ string) (map[string][]byte, error) {
	ids := map[string][]string{}
	err := eachRow(ctx, tx, "SELECT chat, msg_id FROM feed ORDER BY chat, msg_id", func(r *sql.Rows) error {
		var chat string
		var id int64
		if err := r.Scan(&chat, &id); err != nil {
			return err
		}
		ids[chat] = append(ids[chat], strconv.FormatInt(id, 10))
		return nil
	})
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	out := map[string][]byte{}
	for chat, list := range ids {
		out[chat+".ids"] = []byte(strings.Join(list, "\n") + "\n")
	}
	return out, nil
}

// exportEditDates — даты правок (importEditDates наоборот).
func exportEditDates(ctx context.Context, tx *sql.Tx, _ string) ([]byte, error) {
	out := struct {
		Alive json.RawMessage  `json:"alive,omitempty"`
		Seen  map[string]int64 `json:"seen"`
	}{Seen: map[string]int64{}}
	alive, err := kvValue(ctx, tx, KeyEditsAlive)
	if err != nil {
		return nil, err
	}
	out.Alive = alive
	err = eachRow(ctx, tx, "SELECT key, edit_date FROM edit_dates", func(r *sql.Rows) error {
		var k string
		var d int64
		if err := r.Scan(&k, &d); err != nil {
			return err
		}
		out.Seen[k] = d
		return nil
	})
	if err != nil || (alive == nil && len(out.Seen) == 0) {
		return nil, err
	}
	return jsonOut(out)
}

// exportUpdates — отметка обновлений gotd (importUpdates наоборот).
func exportUpdates(ctx context.Context, tx *sql.Tx, _ string) ([]byte, error) {
	head, err := kvValue(ctx, tx, KeyUpdatesState)
	if err != nil || head == nil {
		return nil, err
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(head, &m); err != nil {
		return nil, err
	}
	channels := map[string]int64{}
	hashes := map[string]int64{}
	err = eachRow(ctx, tx, "SELECT channel_id, pts, access_hash FROM update_channels", func(r *sql.Rows) error {
		var id int64
		var pts, hash sql.NullInt64
		if err := r.Scan(&id, &pts, &hash); err != nil {
			return err
		}
		k := strconv.FormatInt(id, 10)
		if pts.Valid {
			channels[k] = pts.Int64
		}
		if hash.Valid {
			hashes[k] = hash.Int64
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	m["channels"], _ = json.Marshal(channels)
	m["access_hashes"], _ = json.Marshal(hashes)
	return jsonOut(m)
}

// exportRoutes — маршруты входящих (importRoutes наоборот).
func exportRoutes(ctx context.Context, tx *sql.Tx, _ string) ([]byte, error) {
	out := map[string]map[string]json.RawMessage{}
	err := eachRow(ctx, tx, "SELECT chat, msg_id, body FROM routes", func(r *sql.Rows) error {
		var chat, body string
		var id int64
		if err := r.Scan(&chat, &id, &body); err != nil {
			return err
		}
		if out[chat] == nil {
			out[chat] = map[string]json.RawMessage{}
		}
		out[chat][strconv.FormatInt(id, 10)] = json.RawMessage(body)
		return nil
	})
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return jsonOut(out)
}

// exportSets — группы чатов (importSets наоборот): состав, постоянные alias
// и чаты, закрытые для групп из-за агента.
func exportSets(ctx context.Context, tx *sql.Tx, _ string) ([]byte, error) {
	out := struct {
		Updated json.RawMessage            `json:"updated,omitempty"`
		Sets    map[string]json.RawMessage `json:"sets"`
		Aliases map[string]string          `json:"aliases"`
		Blocked map[string][]int64         `json:"blocked"`
	}{Sets: map[string]json.RawMessage{}, Aliases: map[string]string{}, Blocked: map[string][]int64{}}
	upd, err := kvValue(ctx, tx, KeySetsUpdated)
	if err != nil {
		return nil, err
	}
	out.Updated = upd
	if err := eachRow(ctx, tx, "SELECT key, body FROM chat_sets", func(r *sql.Rows) error {
		var k, b string
		if err := r.Scan(&k, &b); err != nil {
			return err
		}
		out.Sets[k] = json.RawMessage(b)
		return nil
	}); err != nil {
		return nil, err
	}
	if err := eachRow(ctx, tx, "SELECT chat_id, alias FROM chat_set_aliases", func(r *sql.Rows) error {
		var id, a string
		if err := r.Scan(&id, &a); err != nil {
			return err
		}
		out.Aliases[id] = a
		return nil
	}); err != nil {
		return nil, err
	}
	if err := eachRow(ctx, tx, "SELECT set_key, chat_id FROM chat_set_blocked ORDER BY set_key, chat_id", func(r *sql.Rows) error {
		var k string
		var id int64
		if err := r.Scan(&k, &id); err != nil {
			return err
		}
		out.Blocked[k] = append(out.Blocked[k], id)
		return nil
	}); err != nil {
		return nil, err
	}
	if upd == nil && len(out.Sets) == 0 && len(out.Aliases) == 0 && len(out.Blocked) == 0 {
		return nil, nil
	}
	return jsonOut(out)
}

// exportAgentFolders — папки, заведённые агентом (importAgentFolders наоборот).
func exportAgentFolders(ctx context.Context, tx *sql.Tx, _ string) ([]byte, error) {
	list := []json.RawMessage{}
	err := eachRow(ctx, tx, "SELECT body FROM agent_folders ORDER BY CAST(key AS INTEGER)", func(r *sql.Rows) error {
		var b string
		if err := r.Scan(&b); err != nil {
			return err
		}
		list = append(list, json.RawMessage(b))
		return nil
	})
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return jsonOut(list)
}

// exportNotes — заметки к аккаунтам (importNotes наоборот).
func exportNotes(ctx context.Context, tx *sql.Tx, _ string) ([]byte, error) {
	return exportDocs(TAccountNotes, nil)(ctx, tx, "")
}

// exportUpstream — слежение за обновлениями (importUpstream наоборот).
func exportUpstream(ctx context.Context, tx *sql.Tx, _ string) ([]byte, error) {
	docs, err := readDocs(ctx, tx, "upstream_sources", nil)
	if err != nil || len(docs) == 0 {
		return nil, err
	}
	return jsonOut(map[string]any{"sources": docs})
}

// ExportDocs — Legacy.Export для набора «ключ + JSON» (для пакетов, которые
// регистрируют свои старые файлы).
func ExportDocs(table string) func(context.Context, *sql.Tx, string) ([]byte, error) {
	return exportDocs(table, nil)
}

// exportDocs — набор «ключ + JSON» как map[ключ]тело (insertDocs наоборот);
// plain — тело в виде, который читает старая сборка.
func exportDocs(table string, plain func(json.RawMessage) (json.RawMessage, error)) func(context.Context, *sql.Tx, string) ([]byte, error) {
	return func(ctx context.Context, tx *sql.Tx, _ string) ([]byte, error) {
		docs, err := readDocs(ctx, tx, table, plain)
		if err != nil || len(docs) == 0 {
			return nil, err
		}
		return jsonOut(docs)
	}
}

func readDocs(ctx context.Context, tx *sql.Tx, table string, plain func(json.RawMessage) (json.RawMessage, error)) (map[string]json.RawMessage, error) {
	docs := map[string]json.RawMessage{}
	err := eachRow(ctx, tx, "SELECT key, body FROM "+table, func(r *sql.Rows) error {
		var k, b string
		if err := r.Scan(&k, &b); err != nil {
			return err
		}
		body := json.RawMessage(b)
		if plain != nil {
			var err error
			if body, err = plain(body); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		}
		docs[k] = body
		return nil
	})
	return docs, err
}

// exportKV — одиночное значение kv как файл (kvFile наоборот).
func exportKV(key string) func(context.Context, *sql.Tx, string) ([]byte, error) {
	return func(ctx context.Context, tx *sql.Tx, _ string) ([]byte, error) {
		return kvValue(ctx, tx, key)
	}
}

// kvValue — значение kv (nil — нет).
func kvValue(ctx context.Context, tx *sql.Tx, key string) (json.RawMessage, error) {
	var v string
	err := tx.QueryRowContext(ctx, "SELECT value FROM kv WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(v), nil
}

func eachRow(ctx context.Context, tx *sql.Tx, q string, fn func(*sql.Rows) error) error {
	rows, err := tx.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func unsavedReceipt(dir, id string) (int64, string, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, UnsavedDir, id))
	if err != nil {
		return 0, "", false
	}
	var v struct {
		MessageID int64  `json:"message_id"`
		At        string `json:"at"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return 0, "", false
	}
	return v.MessageID, v.At, true
}
