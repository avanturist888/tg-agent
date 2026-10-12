package tgc

import (
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func edited(id, editDate int) *tg.Message {
	m := &tg.Message{ID: id, PeerID: &tg.PeerUser{UserID: 7}}
	if editDate != 0 {
		m.SetEditDate(editDate)
	}
	return m
}

func TestEditIsNotReaction(t *testing.T) {
	tr := &editTracker{seen: map[string]int{}}
	now := time.Unix(1_800_000_000, 0)
	fresh := int(now.Add(-time.Minute).Unix())
	old := int(now.Add(-time.Hour).Unix())

	if tr.isNew(edited(1, 0), now) {
		t.Fatal("без даты правки — не правка (реакция на неправленное)")
	}
	if !tr.isNew(edited(2, fresh), now) {
		t.Fatal("свежая правка")
	}
	if tr.isNew(edited(2, fresh), now) {
		t.Fatal("та же дата правки — реакция, а не правка")
	}
	if !tr.isNew(edited(2, fresh+30), now) {
		t.Fatal("дата правки сменилась — новая правка")
	}
	if tr.isNew(edited(3, old), now) {
		t.Fatal("давняя правка без известной прежней даты — реакция на правленное сообщение")
	}
	hidden := edited(4, fresh)
	hidden.EditHide = true
	if tr.isNew(hidden, now) {
		t.Fatal("скрытая правка (edit_hide) не будит")
	}
}
