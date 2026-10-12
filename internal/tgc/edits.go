package tgc

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"sync"
	"time"

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
// Даты правок и отметка «служба работала до» живут в data/edit_dates.json.

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
	since time.Time      // служба не работала с этого момента (прошлая остановка)
	path  string
	dirty bool
}

type editsFile struct {
	Alive int64          `json:"alive"` // последний раз служба была жива (unix)
	Seen  map[string]int `json:"seen"`
}

var edits = &editTracker{seen: map[string]int{}}

// open — даты правок и время прошлой остановки службы с диска.
func (t *editTracker) open(path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.path = path
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var f editsFile
	if json.Unmarshal(raw, &f) != nil {
		return
	}
	if f.Seen != nil {
		t.seen = f.Seen
	}
	if f.Alive > 0 {
		t.since = time.Unix(f.Alive, 0).Add(-editsSlack)
	}
}

func (t *editTracker) save(now time.Time) {
	t.mu.Lock()
	if t.path == "" {
		t.mu.Unlock()
		return
	}
	raw, err := json.Marshal(editsFile{Alive: now.Unix(), Seen: t.seen})
	path := t.path
	t.dirty = false
	t.mu.Unlock()
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, path)
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
	t.dirty = true
	if known {
		return msg.EditDate != last
	}
	at := time.Unix(int64(msg.EditDate), 0)
	return now.Sub(at) < editFresh || (!t.since.IsZero() && !at.Before(t.since))
}
