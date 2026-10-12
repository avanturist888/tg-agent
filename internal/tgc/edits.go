package tgc

import (
	"strconv"
	"sync"
	"time"

	"github.com/gotd/td/tg"
)

// Реакции в личках и обычных группах Telegram присылает как правку
// сообщения (updateEditMessage), и дата правки у него остаётся прежней. У
// когда-то правленного сообщения она есть, поэтому любая реакция — агента,
// владельца, собеседника — выглядела бы новой правкой. Новая правка — та,
// у которой дата правки сменилась; если прежней мы не знаем (служба только
// запустилась), — свежая.

// editFresh — правку старше этого без известной прежней даты считаем
// реакцией или другим служебным обновлением, а не правкой.
const editFresh = 15 * time.Minute

type editTracker struct {
	mu   sync.Mutex
	seen map[string]int // peer:id → edit_date, о которой уже знаем
}

var edits = &editTracker{seen: map[string]int{}}

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
	if len(t.seen) > 50000 {
		clear(t.seen)
	}
	t.seen[key] = msg.EditDate
	if known {
		return msg.EditDate != last
	}
	return now.Sub(time.Unix(int64(msg.EditDate), 0)) < editFresh
}
