package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations — схема по версиям: migrations[i] переводит базу с версии i на
// i+1 (PRAGMA user_version). Добавлять только в конец, старые не менять:
// их уже применили у пользователей.
//
// Таблицы двух видов. У данных, к которым ходят по ключу и по одной
// записи, — свои столбцы (черновики, ленты, расшифровки, кэши Telegram,
// маршруты). Мелкие наборы сущностей, чья форма меняется вместе с кодом
// (подписки, запросы доступа, карточки…), — «ключ + JSON» (Table): запись
// по-прежнему атомарна и по одной строке, а новое поле не требует миграции.
// Одиночные значения — в kv.
var migrations = []string{
	// 1 — переезд с JSON-файлов
	`
CREATE TABLE kv (
	key     TEXT PRIMARY KEY,
	value   TEXT NOT NULL CHECK (json_valid(value)),
	updated INTEGER NOT NULL
) WITHOUT ROWID;

-- какие старые файлы уже перенесены (legacy.go)
CREATE TABLE legacy_imports (
	file     TEXT PRIMARY KEY,
	imported INTEGER NOT NULL,
	size     INTEGER NOT NULL,
	mtime    INTEGER NOT NULL,
	rows     INTEGER NOT NULL,
	moved_to TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;

-- черновики (outbox): body — черновик целиком (JSON, с полями от других
-- версий), столбцы рядом — для выборок; sender_* — кто его сейчас отправляет
CREATE TABLE drafts (
	seq          INTEGER PRIMARY KEY AUTOINCREMENT,
	id           TEXT NOT NULL UNIQUE,
	status       TEXT NOT NULL,
	expires      INTEGER NOT NULL DEFAULT 0,
	send_at      INTEGER,
	sender_pid   INTEGER,
	sender_since REAL,
	body         TEXT NOT NULL CHECK (json_valid(body))
);
CREATE INDEX drafts_by_status ON drafts(status, send_at);

-- ленты новых сообщений: чат → id сообщений
CREATE TABLE feed (
	chat   TEXT NOT NULL,
	msg_id INTEGER NOT NULL,
	PRIMARY KEY (chat, msg_id)
) WITHOUT ROWID;

-- расшифровки голосовых: «чат:сообщение» → текст
CREATE TABLE transcripts (
	key      TEXT PRIMARY KEY,
	text     TEXT NOT NULL,
	engine   TEXT NOT NULL,
	language TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;

-- access_hash пользователей и каналов, @username → "user:ID" / "channel:ID"
CREATE TABLE peer_users     (id INTEGER PRIMARY KEY, access_hash INTEGER NOT NULL);
CREATE TABLE peer_channels  (id INTEGER PRIMARY KEY, access_hash INTEGER NOT NULL);
CREATE TABLE peer_usernames (name TEXT PRIMARY KEY, ref TEXT NOT NULL) WITHOUT ROWID;

-- последняя известная дата правки сообщения («peer:id»); seen — когда узнали
CREATE TABLE edit_dates (
	key       TEXT PRIMARY KEY,
	edit_date INTEGER NOT NULL,
	seen      INTEGER NOT NULL
) WITHOUT ROWID;
CREATE INDEX edit_dates_by_seen ON edit_dates(seen);

-- отметка обновлений gotd по каналам (общая — в kv updates_state)
CREATE TABLE update_channels (
	channel_id  INTEGER PRIMARY KEY,
	pts         INTEGER,
	access_hash INTEGER
);

-- маршруты входящих (кому отвечать), без текста
CREATE TABLE routes (
	chat   TEXT NOT NULL,
	msg_id INTEGER NOT NULL,
	at     INTEGER NOT NULL,
	body   TEXT NOT NULL CHECK (json_valid(body)),
	PRIMARY KEY (chat, msg_id)
) WITHOUT ROWID;

-- наборы «ключ + JSON» (Table)
CREATE TABLE subscriptions    (key TEXT PRIMARY KEY, body TEXT NOT NULL CHECK (json_valid(body))) WITHOUT ROWID;
CREATE TABLE access_requests  (key TEXT PRIMARY KEY, body TEXT NOT NULL CHECK (json_valid(body))) WITHOUT ROWID;
CREATE TABLE bot_ops          (key TEXT PRIMARY KEY, body TEXT NOT NULL CHECK (json_valid(body))) WITHOUT ROWID;
CREATE TABLE bots             (key TEXT PRIMARY KEY, body TEXT NOT NULL CHECK (json_valid(body))) WITHOUT ROWID;
CREATE TABLE pinned_drafts    (key TEXT PRIMARY KEY, body TEXT NOT NULL CHECK (json_valid(body))) WITHOUT ROWID;
CREATE TABLE t3_threads       (key TEXT PRIMARY KEY, body TEXT NOT NULL CHECK (json_valid(body))) WITHOUT ROWID;
CREATE TABLE upstream_sources (key TEXT PRIMARY KEY, body TEXT NOT NULL CHECK (json_valid(body))) WITHOUT ROWID;
CREATE TABLE upstream_cards   (key TEXT PRIMARY KEY, body TEXT NOT NULL CHECK (json_valid(body))) WITHOUT ROWID;
CREATE TABLE agent_folders    (key TEXT PRIMARY KEY, body TEXT NOT NULL CHECK (json_valid(body))) WITHOUT ROWID;
CREATE TABLE account_notes    (key TEXT PRIMARY KEY, body TEXT NOT NULL CHECK (json_valid(body))) WITHOUT ROWID;
CREATE TABLE chat_sets        (key TEXT PRIMARY KEY, body TEXT NOT NULL CHECK (json_valid(body))) WITHOUT ROWID;

-- постоянные alias чатов из групп и чаты, закрытые для групп из-за агента
CREATE TABLE chat_set_aliases (chat_id TEXT PRIMARY KEY, alias TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE chat_set_blocked (
	set_key TEXT NOT NULL,
	chat_id INTEGER NOT NULL,
	PRIMARY KEY (set_key, chat_id)
) WITHOUT ROWID;
`,
	// 2: сколько раз черновик брали в отправку (outbox.ClaimSend) — пишется
	// до первого байта в сеть. Была попытка, а итога нет — могло уйти:
	// автоматически такой черновик больше не отправляется.
	// Захват, оставшийся от схемы 1 (sender_pid есть), — тоже попытка: его
	// процесс мог умереть, уже отправив.
	`ALTER TABLE drafts ADD COLUMN send_attempts INTEGER NOT NULL DEFAULT 0;
UPDATE drafts SET send_attempts = 1 WHERE sender_pid IS NOT NULL;`,
	// 3: итоги карточек черновиков — номер последнего принятого, его текст и
	// показан ли он: не показанный (Bot API не отвечал) уборка повторяет, а
	// итог, опоздавший к локу, по номеру отвергается (core/approvals.go)
	`CREATE TABLE card_states (key TEXT PRIMARY KEY, body TEXT NOT NULL CHECK (json_valid(body))) WITHOUT ROWID;`,
	// 4: файл выгружен из базы для отката (tg rollback, rollback.go): при
	// следующем переносе он главнее базы — его вела старая сборка
	`ALTER TABLE legacy_imports ADD COLUMN exported INTEGER NOT NULL DEFAULT 0;`,
	// 5: хеш выгруженного откатом содержимого — пишется до самого файла:
	// повтор прерванной выгрузки узнаёт свой файл по нему
	`ALTER TABLE legacy_imports ADD COLUMN export_hash TEXT NOT NULL DEFAULT '';`,
}

// Version — версия схемы, которую знает эта сборка.
func Version() int { return len(migrations) }

func (db *DB) migrate() error {
	ctx := context.Background()
	return db.write(ctx, true, func(tx *sql.Tx) error {
		var v int
		if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
			return err
		}
		if v > len(migrations) {
			return fmt.Errorf("%w (версия схемы %d, эта сборка знает до %d)", ErrNewerSchema, v, len(migrations))
		}
		for ; v < len(migrations); v++ {
			if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
				return fmt.Errorf("миграция %d: %w", v+1, err)
			}
		}
		// PRAGMA не принимает параметров; v — число из кода
		_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v))
		return err
	})
}
