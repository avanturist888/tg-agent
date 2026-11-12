package store

// Откат на сборку до state.db (tg rollback).
//
// Отложенные *.migrated — снимки на момент переноса: всё, что было потом,
// есть только в базе. Вернуть снимок очереди черновиков значило бы отдать
// старой сборке уже отправленное как неотправленное (она отправила бы его
// снова) и отменённое как ждущее. Поэтому очередь выгружается из базы
// (outbox.json в формате старой сборки — тело черновика в базе и есть его
// JSON), а прочие снимки возвращаются как есть: в них нет ничего, что
// старая сборка отправила бы получателю.
//
// Попытку отправки без итога старая сборка различать не умеет: approved без
// send_error она дослала бы при старте. Такой черновик выгружается со
// сбоем «могло уйти» — она его не тронет, повтор — только явный. Ушедшее, чья
// отметка не легла в базу (sent-unsaved), выгружается отправленным.
//
// После выгрузки (отметка RollbackFile) эта сборка не берёт черновики в
// отправку и не переносит старые файлы: иначе отправленное ею после
// выгрузки старая сборка не увидела бы. Снимает отметку обновление —
// tg cutover (ClearRollback).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// exportedFile — файл, который выгружается из базы, а не возвращается
// снимком: снимок здесь устарел бы с последствиями — повторная отправка
// (очередь), открытый агенту чат, заблокированный после переноса (группы
// чатов), удаление агентом чужой папки с номером его прежней (папки агента),
// потерянные заметки. Остальные файлы — кэши и служебные отметки: их снимок
// безопасен.
type exportedFile struct {
	file  string
	build func(ctx context.Context, tx *sql.Tx, dir string) (any, error)
}

var exported = []exportedFile{
	{"outbox.json", exportOutbox},
	{"chat_sets.json", exportSets},
	{"agent_folders.json", exportAgentFolders},
	{"account_notes.json", exportNotes},
}

// ExportLegacy — выгрузить состояние data/ dir для старой сборки: что в
// exported — из базы, прочие перенесённые файлы — обратно из *.migrated.
// Процессы установки должны быть остановлены (tg rollback это проверяет);
// живой отправитель черновика — отказ. Прерванную выгрузку можно повторить:
// файл, выгруженный ею же (размер и время записаны в legacy_imports),
// перезаписывается, а чужой (его уже вела старая сборка) — отказ. Сбой
// возврата одного снимка не мешает остальным. Возвращает, что легло.
func ExportLegacy(dir string) ([]string, error) {
	db, err := Open(dir)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	for _, e := range exported {
		if p := filepath.Join(dir, e.file); !ownExport(ctx, db, e.file, p) {
			return nil, fmt.Errorf("%s уже лежит, и выгружала его не эта выгрузка (ещё не перенесён или его вела старая сборка) — поверх него нельзя", p)
		}
	}
	// сначала отметка: с ней эта сборка уже ничего не берёт в отправку
	raw, _ := json.Marshal(map[string]any{"at": time.Now().Format(time.RFC3339)})
	if err := fsx.WriteFile(filepath.Join(dir, RollbackFile), raw, 0o600); err != nil {
		return nil, err
	}
	if err := RevokeCutover(dir); err != nil {
		return nil, err
	}
	bodies := make([][]byte, len(exported))
	// транзакция записи — чтобы никто не поменял базу, пока её читаем
	err = db.Write(ctx, func(tx *sql.Tx) error {
		for i, e := range exported {
			v, err := e.build(ctx, tx, dir)
			if err != nil {
				return fmt.Errorf("%s: %w", e.file, err)
			}
			if bodies[i], err = json.MarshalIndent(v, "", "  "); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var done []string
	for i, e := range exported {
		p := filepath.Join(dir, e.file)
		if err := fsx.WriteFile(p, bodies[i], 0o600); err != nil {
			return done, err
		}
		st, err := os.Stat(p)
		if err != nil {
			return done, err
		}
		if _, err := db.Q().ExecContext(ctx, `INSERT INTO legacy_imports(file, imported, size, mtime, rows, exported) VALUES(?, ?, ?, ?, 0, 1)
			ON CONFLICT(file) DO UPDATE SET size = excluded.size, mtime = excluded.mtime, exported = 1`,
			e.file, time.Now().Unix(), st.Size(), st.ModTime().UnixNano()); err != nil {
			return done, err
		}
		done = append(done, e.file+" (из базы)")
	}
	moves, err := snapshots(ctx, db)
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

// ownExport — файла нет или его выгрузила эта же выгрузка (не тронут после).
func ownExport(ctx context.Context, db *DB, file, path string) bool {
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	var size, mtime, exp int64
	err = db.Q().QueryRowContext(ctx, "SELECT size, mtime, exported FROM legacy_imports WHERE file = ?", file).Scan(&size, &mtime, &exp)
	return err == nil && exp != 0 && size == st.Size() && mtime == st.ModTime().UnixNano()
}

type snapshot struct{ file, from string }

// snapshots — перенесённые файлы, которые возвращаются снимком.
func snapshots(ctx context.Context, db *DB) ([]snapshot, error) {
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
		if !slices.ContainsFunc(exported, func(e exportedFile) bool { return e.file == m.file }) {
			out = append(out, m)
		}
	}
	return out, rows.Err()
}

// exportOutbox — очередь в формате старой сборки. Попытка без итога — со
// сбоем «могло уйти» (старая сборка дослала бы approved без send_error),
// ушедшее без отметки (sent-unsaved) — отправленным.
func exportOutbox(ctx context.Context, tx *sql.Tx, dir string) (any, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id, status, body, send_attempts, sender_pid, sender_since FROM drafts ORDER BY seq")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	drafts := []map[string]any{}
	for rows.Next() {
		var (
			id, status, body string
			attempts         int
			pid              sql.NullInt64
			since            sql.NullFloat64
		)
		if err := rows.Scan(&id, &status, &body, &attempts, &pid, &since); err != nil {
			return nil, err
		}
		if pid.Valid && lock.PIDAlive(int(pid.Int64), since.Float64) {
			return nil, fmt.Errorf("черновик %s сейчас отправляет процесс %d — останови его и повтори", id, pid.Int64)
		}
		m := map[string]any{}
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			return nil, fmt.Errorf("черновик %s: %w", id, err)
		}
		m["status"] = status
		if mid, at, ok := unsavedReceipt(dir, id); ok && status != "sent" {
			// ушло, отметка не легла (могло и протухнуть, пока ждали)
			m["status"], m["message_id"], m["sent_at"] = "sent", mid, at
			delete(m, "send_error")
		} else if status == "approved" && attempts > 0 && m["send_error"] == nil {
			m["send_error"] = uncertainSend
		}
		drafts = append(drafts, m)
	}
	return drafts, rows.Err()
}

// exportSets — группы чатов (importSets наоборот): состав, постоянные alias
// и чаты, закрытые для групп из-за агента.
func exportSets(ctx context.Context, tx *sql.Tx, _ string) (any, error) {
	out := struct {
		Updated json.RawMessage            `json:"updated,omitempty"`
		Sets    map[string]json.RawMessage `json:"sets"`
		Aliases map[string]string          `json:"aliases"`
		Blocked map[string][]int64         `json:"blocked"`
	}{Sets: map[string]json.RawMessage{}, Aliases: map[string]string{}, Blocked: map[string][]int64{}}
	var upd string
	switch err := tx.QueryRowContext(ctx, "SELECT value FROM kv WHERE key = ?", KeySetsUpdated).Scan(&upd); {
	case err == nil:
		out.Updated = json.RawMessage(upd)
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
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
	return out, nil
}

// exportAgentFolders — папки, заведённые агентом (importAgentFolders наоборот).
func exportAgentFolders(ctx context.Context, tx *sql.Tx, _ string) (any, error) {
	list := []json.RawMessage{}
	err := eachRow(ctx, tx, "SELECT body FROM agent_folders ORDER BY CAST(key AS INTEGER)", func(r *sql.Rows) error {
		var b string
		if err := r.Scan(&b); err != nil {
			return err
		}
		list = append(list, json.RawMessage(b))
		return nil
	})
	return list, err
}

// exportNotes — заметки к аккаунтам (importNotes наоборот).
func exportNotes(ctx context.Context, tx *sql.Tx, _ string) (any, error) {
	m := map[string]json.RawMessage{}
	err := eachRow(ctx, tx, "SELECT key, body FROM "+TAccountNotes, func(r *sql.Rows) error {
		var k, b string
		if err := r.Scan(&k, &b); err != nil {
			return err
		}
		m[k] = json.RawMessage(b)
		return nil
	})
	return m, err
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
