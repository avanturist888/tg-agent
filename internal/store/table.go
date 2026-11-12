package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Таблицы «ключ + JSON» (schema.go).
const (
	TSubscriptions   = "subscriptions"
	TAccessRequests  = "access_requests"
	TBotOps          = "bot_ops"
	TBots            = "bots"
	TPinnedDrafts    = "pinned_drafts"
	TT3Threads       = "t3_threads"
	TUpstreamSources = "upstream_sources"
	TUpstreamCards   = "upstream_cards"
	TAgentFolders    = "agent_folders"
	TAccountNotes    = "account_notes"
	TChatSets        = "chat_sets"
	TPendingCards    = "pending_cards"
)

var docTables = map[string]bool{
	TSubscriptions: true, TAccessRequests: true, TBotOps: true, TBots: true, TPinnedDrafts: true,
	TT3Threads: true, TUpstreamSources: true, TUpstreamCards: true, TAgentFolders: true,
	TAccountNotes: true, TChatSets: true, TPendingCards: true,
}

// Table — набор записей V по строковому ключу; V хранится JSON.
type Table[V any] struct{ name string }

// NewTable — таблица из списка выше (имя попадает в SQL, поэтому только оттуда).
func NewTable[V any](name string) Table[V] {
	if !docTables[name] {
		panic("store: неизвестная таблица " + name)
	}
	return Table[V]{name: name}
}

func (t Table[V]) rows(ctx context.Context, q Querier) (map[string]string, error) {
	rs, err := q.QueryContext(ctx, "SELECT key, body FROM "+t.name)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[string]string{}
	for rs.Next() {
		var k, b string
		if err := rs.Scan(&k, &b); err != nil {
			return nil, err
		}
		out[k] = b
	}
	return out, rs.Err()
}

// All — все записи. Нечитаемая запись (её записала сборка с другой формой)
// — ошибка: молча потерять её при следующем Edit нельзя.
func (t Table[V]) All(ctx context.Context, q Querier) (map[string]V, error) {
	raw, err := t.rows(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make(map[string]V, len(raw))
	for k, b := range raw {
		var v V
		if err := json.Unmarshal([]byte(b), &v); err != nil {
			return nil, fmt.Errorf("%s[%s]: %w", t.name, k, err)
		}
		out[k] = v
	}
	return out, nil
}

// Get — запись по ключу; false — нет.
func (t Table[V]) Get(ctx context.Context, q Querier, key string) (V, bool, error) {
	var (
		v V
		b string
	)
	err := q.QueryRowContext(ctx, "SELECT body FROM "+t.name+" WHERE key = ?", key).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return v, false, nil
	}
	if err != nil {
		return v, false, err
	}
	if err := json.Unmarshal([]byte(b), &v); err != nil {
		return v, false, fmt.Errorf("%s[%s]: %w", t.name, key, err)
	}
	return v, true, nil
}

// Put — записать (заменить) запись.
func (t Table[V]) Put(ctx context.Context, q Querier, key string, v V) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return t.put(ctx, q, key, string(b))
}

func (t Table[V]) put(ctx context.Context, q Querier, key, body string) error {
	_, err := q.ExecContext(ctx,
		"INSERT INTO "+t.name+"(key, body) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET body = excluded.body", key, body)
	return err
}

// Insert — добавить запись, если такой ещё нет (перенос старых файлов:
// база главнее). false — запись уже была.
func (t Table[V]) Insert(ctx context.Context, q Querier, key string, v V) (bool, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return false, err
	}
	r, err := q.ExecContext(ctx, "INSERT OR IGNORE INTO "+t.name+"(key, body) VALUES(?, ?)", key, string(b))
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n > 0, nil
}

// Delete — убрать запись.
func (t Table[V]) Delete(ctx context.Context, q Querier, key string) error {
	_, err := q.ExecContext(ctx, "DELETE FROM "+t.name+" WHERE key = ?", key)
	return err
}

// EditTx — Edit внутри уже открытой транзакции записи.
func (t Table[V]) EditTx(ctx context.Context, tx *sql.Tx, fn func(m map[string]V) (bool, error)) error {
	raw, err := t.rows(ctx, tx)
	if err != nil {
		return err
	}
	m := make(map[string]V, len(raw))
	for k, b := range raw {
		var v V
		if err := json.Unmarshal([]byte(b), &v); err != nil {
			return fmt.Errorf("%s[%s]: %w", t.name, k, err)
		}
		m[k] = v
	}
	changed, err := fn(m)
	if err != nil || !changed {
		return err
	}
	for k, v := range m {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if old, ok := raw[k]; ok && old == string(b) {
			continue
		}
		if err := t.put(ctx, tx, k, string(b)); err != nil {
			return err
		}
	}
	for k := range raw {
		if _, ok := m[k]; !ok {
			if err := t.Delete(ctx, tx, k); err != nil {
				return err
			}
		}
	}
	return nil
}

// Edit — прочитать все записи, поменять fn и записать изменённые одной
// транзакцией: между чтением и записью никто не вклинится (и из другого
// процесса). fn вернула false — ничего не пишем. Записываются только
// изменившиеся и удалённые записи.
func (t Table[V]) Edit(ctx context.Context, db *DB, fn func(m map[string]V) bool) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		return t.EditTx(ctx, tx, func(m map[string]V) (bool, error) { return fn(m), nil })
	})
}
