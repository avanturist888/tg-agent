package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Ключи одиночных значений (kv). Все — здесь, чтобы два пакета не взяли
// один ключ.
const (
	KeyBotOffset      = "bot_offset"      // смещение getUpdates бота и id бота
	KeyUpstreamThread = "upstream_thread" // тред агента для обновлений MAX
	KeyAccount        = "account"         // чей аккаунт у профиля (имя, @username)
	KeyEditsAlive     = "edits_alive"     // служба была жива до (unix), для правок
	KeyPeersSelf      = "peers_self"      // id своего пользователя Telegram
	KeyUpdatesState   = "updates_state"   // общая отметка обновлений gotd
	KeySetsUpdated    = "chat_sets_updated"
)

// GetKV — значение по ключу в v; false — его нет.
func GetKV(ctx context.Context, q Querier, key string, v any) (bool, error) {
	var raw string
	err := q.QueryRowContext(ctx, "SELECT value FROM kv WHERE key = ?", key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), v)
}

// PutKV — записать значение.
func PutKV(ctx context.Context, q Querier, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx,
		"INSERT INTO kv(key, value, updated) VALUES(?, ?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated = excluded.updated",
		key, string(raw), time.Now().Unix())
	return err
}

// DelKV — убрать значение.
func DelKV(ctx context.Context, q Querier, key string) error {
	_, err := q.ExecContext(ctx, "DELETE FROM kv WHERE key = ?", key)
	return err
}
