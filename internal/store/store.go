// Package store — состояние профиля в SQLite (data/state.db).
//
// Раньше каждое хранилище было своим JSON-файлом: прочитать целиком →
// поменять → записать целиком под файловым локом (или вовсе под мьютексом
// одного процесса). Это не давало ни атомарности между процессами (служба,
// tg tool-call без службы, tgw send-due, окно), ни изменения одной записи
// без перезаписи всего файла, а сбой питания 9.11.2026 обнулил outbox.json.
//
// Здесь одна база на профиль: WAL (читатели не ждут писателя), запись — только
// транзакцией BEGIN IMMEDIATE (Write): она сразу берёт право записи, поэтому
// «прочитать → решить → записать» внутри неё атомарно для всех процессов.
// synchronous=FULL: одобрение и отправка черновика переживают сбой питания.
//
// В базе нет секретов: сессии (Telegram, MAX, WhatsApp) и токены остаются в
// своих файлах и в диспетчере учётных данных — state.db можно смотреть и
// копировать для разбора, не раздавая доступ к аккаунтам.
//
// Схема — версии в schema.go (PRAGMA user_version). Старые JSON-файлы
// установок до переезда импортирует Open (legacy.go) и откладывает рядом.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// FileName — имя базы в data/ профиля.
const FileName = "state.db"

// busyTimeout — сколько транзакция ждёт, пока другой процесс допишет свою.
// Транзакции короткие (никаких сетевых вызовов внутри), так что ожидание
// дольше нескольких секунд — уже авария.
const busyTimeout = 15 * time.Second

// DB — база одного профиля.
type DB struct {
	Dir     string // data/ профиля
	sql     *sql.DB
	pending atomic.Bool // перенос старых файлов отложен (LegacyPending)
}

var (
	openMu sync.Mutex
	opened = map[string]*DB{}
)

// Open — база в каталоге dir (data/ профиля). Одна на процесс и каталог:
// повторный вызов отдаёт уже открытую. При первом открытии — схема до
// текущей версии и импорт старых JSON-файлов.
func Open(dir string) (*DB, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	openMu.Lock()
	defer openMu.Unlock()
	if db := opened[abs]; db != nil {
		return db, nil
	}
	db, err := open(abs)
	if err != nil {
		return nil, err
	}
	opened[abs] = db
	return db, nil
}

func open(dir string) (*DB, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Set("_txlock", "immediate")
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, FileName))+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if testing.Testing() {
		// тесты кладут базу во временный каталог, который потом удаляют: на
		// Windows открытый файл не удалить, поэтому соединения не копим
		raw.SetMaxIdleConns(0)
	}
	db := &DB{Dir: dir, sql: raw}
	if err := db.migrate(); err != nil {
		raw.Close()
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, FileName), err)
	}
	if err := db.ImportLegacy(); err != nil {
		raw.Close()
		return nil, fmt.Errorf("%s: импорт старых файлов: %w", filepath.Join(dir, FileName), err)
	}
	return db, nil
}

// Write — транзакция записи. fn видит базу целиком и меняет её атомарно:
// другой процесс не вклинится между чтением и записью. Внутри fn — никаких
// сетевых вызовов: транзакция держит запись для всех процессов профиля.
func (db *DB) Write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := db.sql.BeginTx(ctx, nil) // _txlock=immediate → BEGIN IMMEDIATE
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Q — для чтения одним запросом (без транзакции: в WAL он видит
// согласованный снимок и никого не держит).
func (db *DB) Q() *sql.DB { return db.sql }

// Querier — то, чем читают: и *sql.DB, и *sql.Tx.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// closeAll — закрыть все базы процесса (тесты).
func closeAll() {
	openMu.Lock()
	defer openMu.Unlock()
	for k, db := range opened {
		_ = db.sql.Close()
		delete(opened, k)
	}
}

// ErrNewerSchema — база от более новой сборки tg-agent.
var ErrNewerSchema = errors.New("state.db записана более новой версией tg-agent — обнови tg-agent")
