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

// ExportLegacy — выгрузить состояние data/ dir для старой сборки: очередь —
// в outbox.json, прочие перенесённые файлы — обратно из *.migrated. Процессы
// установки должны быть остановлены (tg rollback это проверяет); живой
// отправитель черновика — отказ. Возвращает, что вернулось на место.
func ExportLegacy(dir string) ([]string, error) {
	outbox := filepath.Join(dir, "outbox.json")
	if _, err := os.Stat(outbox); err == nil {
		return nil, fmt.Errorf("%s уже лежит (ещё не перенесён в базу) — выгружать поверх него нельзя", outbox)
	}
	db, err := Open(dir)
	if err != nil {
		return nil, err
	}
	// сначала отметка: с ней эта сборка уже ничего не берёт в отправку
	raw, _ := json.Marshal(map[string]any{"at": time.Now().Format(time.RFC3339)})
	if err := fsx.WriteFile(filepath.Join(dir, RollbackFile), raw, 0o600); err != nil {
		return nil, err
	}
	if err := RevokeCutover(dir); err != nil {
		return nil, err
	}
	ctx := context.Background()
	var drafts []map[string]any
	// транзакция записи — чтобы никто не поменял очередь, пока её читаем
	err = db.Write(ctx, func(tx *sql.Tx) error {
		drafts = nil
		rows, err := tx.QueryContext(ctx, "SELECT id, status, body, send_attempts, sender_pid, sender_since FROM drafts ORDER BY seq")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				id, status, body string
				attempts         int
				pid              sql.NullInt64
				since            sql.NullFloat64
			)
			if err := rows.Scan(&id, &status, &body, &attempts, &pid, &since); err != nil {
				return err
			}
			if pid.Valid && lock.PIDAlive(int(pid.Int64), since.Float64) {
				return fmt.Errorf("черновик %s сейчас отправляет процесс %d — останови его и повтори", id, pid.Int64)
			}
			m := map[string]any{}
			if err := json.Unmarshal([]byte(body), &m); err != nil {
				return fmt.Errorf("черновик %s: %w", id, err)
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
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if drafts == nil {
		drafts = []map[string]any{}
	}
	body, err := json.MarshalIndent(drafts, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := fsx.WriteFile(outbox, body, 0o600); err != nil {
		return nil, err
	}
	restored := []string{"outbox.json (из базы)"}
	rows, err := db.Q().QueryContext(ctx, "SELECT file, moved_to FROM legacy_imports WHERE moved_to != '' AND file != 'outbox.json'")
	if err != nil {
		return restored, err
	}
	type move struct{ file, from string }
	var moves []move
	for rows.Next() {
		var m move
		if err := rows.Scan(&m.file, &m.from); err != nil {
			rows.Close()
			return restored, err
		}
		moves = append(moves, m)
	}
	rows.Close()
	for _, m := range moves {
		dst := filepath.Join(dir, filepath.FromSlash(m.file))
		src := filepath.Join(filepath.Dir(dst), m.from)
		if _, err := os.Stat(dst); err == nil {
			continue // старая сборка уже завела новый
		}
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			return restored, fmt.Errorf("%s не вернуть на место: %w", m.file, err)
		}
		restored = append(restored, m.file)
	}
	return restored, nil
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
