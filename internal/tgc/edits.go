package tgc

import (
	"context"
	"database/sql"
	"strconv"
	"sync"
	"time"

	"tgagent/internal/store"

	"github.com/gotd/td/tg"
)

// Реакции в личках и обычных группах Telegram присылает как правку
// сообщения (updateEditMessage), и дата правки у него остаётся прежней. У
// когда-то правленного сообщения она есть, поэтому любая реакция — агента,
// владельца, собеседника — выглядела бы новой правкой. Новая правка — та,
// у которой дата правки сменилась. Если прежней мы не знаем — правка
// свежая (моложе editFresh) или сделана, пока служба не работала (позже
// её прошлой остановки: такие приходят при догонке пропущенного).
//
// Даты правок (таблица edit_dates) и отметка «служба работала до» (kv
// edits_alive) живут в data/state.db; в памяти — всё, на диск — изменения
// раз в editsFlush. Пишет только служба.

// editFresh — правку старше этого без известной прежней даты (и не из
// простоя службы) считаем реакцией или другим служебным обновлением.
const editFresh = 15 * time.Minute

const (
	editsFlush = 30 * time.Second
	editsMax   = 50000
	// часы машины и Telegram расходятся — окно простоя берём с запасом
	editsSlack = 2 * time.Minute
)

type editTracker struct {
	mu    sync.Mutex
	seen  map[string]int // peer:id → edit_date, о которой уже знаем
	dirty map[string]int // что записать со следующим save
	since time.Time      // служба не работала с этого момента (прошлая остановка)
	dir   string
}

var edits = &editTracker{seen: map[string]int{}, dirty: map[string]int{}}

// open — даты правок и время прошлой остановки службы из базы.
func (t *editTracker) open(dir string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dir = dir
	db, err := store.Open(dir)
	if err != nil {
		return
	}
	ctx := context.Background()
	var alive int64
	if ok, _ := store.GetKV(ctx, db.Q(), store.KeyEditsAlive, &alive); ok && alive > 0 {
		t.since = time.Unix(alive, 0).Add(-editsSlack)
	}
	rows, err := db.Q().QueryContext(ctx, "SELECT key, edit_date FROM edit_dates ORDER BY seen DESC LIMIT ?", editsMax)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var (
			k string
			d int
		)
		if rows.Scan(&k, &d) == nil {
			t.seen[k] = d
		}
	}
}

func (t *editTracker) save(now time.Time) {
	t.mu.Lock()
	if t.dir == "" {
		t.mu.Unlock()
		return
	}
	dirty := t.dirty
	t.dirty = map[string]int{}
	dir := t.dir
	t.mu.Unlock()
	db, err := store.Open(dir)
	if err == nil {
		ctx := context.Background()
		err = db.Write(ctx, func(tx *sql.Tx) error {
			for k, d := range dirty {
				if _, err := tx.ExecContext(ctx, `INSERT INTO edit_dates(key, edit_date, seen) VALUES(?, ?, ?)
					ON CONFLICT(key) DO UPDATE SET edit_date = excluded.edit_date, seen = excluded.seen`,
					k, d, now.Unix()); err != nil {
					return err
				}
			}
			if len(dirty) > 0 {
				// старше editsMax последних — не нужны: такую правку без
				// известной даты и так отсекут editFresh и since
				if _, err := tx.ExecContext(ctx, `DELETE FROM edit_dates WHERE key IN
					(SELECT key FROM edit_dates ORDER BY seen DESC LIMIT -1 OFFSET ?)`, editsMax); err != nil {
					return err
				}
			}
			return store.PutKV(ctx, tx, store.KeyEditsAlive, now.Unix())
		})
	}
	if err != nil {
		t.mu.Lock()
		for k, d := range dirty {
			if _, newer := t.dirty[k]; !newer {
				t.dirty[k] = d
			}
		}
		t.mu.Unlock()
	}
}

// keep — отмечать на диске, что служба жива (и сбрасывать даты правок).
func (t *editTracker) keep(ctx context.Context) {
	t.save(time.Now())
	for ctx.Err() == nil {
		sleep(ctx, editsFlush)
		t.save(time.Now())
	}
	t.save(time.Now())
}

// isNew — у сообщения новая видимая правка (не повтор той же и не реакция).
func (t *editTracker) isNew(m tg.MessageClass, now time.Time) bool {
	msg, ok := m.(*tg.Message)
	if !ok || !visibleEdit(m) {
		return false
	}
	key := peerKey(msg.PeerID) + ":" + strconv.Itoa(msg.ID)
	t.mu.Lock()
	defer t.mu.Unlock()
	last, known := t.seen[key]
	if len(t.seen) > editsMax {
		clear(t.seen)
	}
	t.seen[key] = msg.EditDate
	if t.dirty == nil {
		t.dirty = map[string]int{}
	}
	t.dirty[key] = msg.EditDate
	if known {
		return msg.EditDate != last
	}
	at := time.Unix(int64(msg.EditDate), 0)
	return now.Sub(at) < editFresh || (!t.since.IsZero() && !at.Before(t.since))
}
