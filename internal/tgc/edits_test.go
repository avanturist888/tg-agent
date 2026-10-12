package tgc

import (
	"path/filepath"
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

func TestEditsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edit_dates.json")
	stop := time.Unix(1_800_000_000, 0)
	first := &editTracker{seen: map[string]int{}}
	first.open(path)
	if !first.isNew(edited(1, int(stop.Add(-5*time.Minute).Unix())), stop) {
		t.Fatal("свежая правка")
	}
	first.save(stop) // служба остановилась

	restart := stop.Add(3 * time.Hour)
	again := &editTracker{seen: map[string]int{}}
	again.open(path)
	if again.isNew(edited(1, int(stop.Add(-5*time.Minute).Unix())), restart) {
		t.Fatal("та же дата правки после перезапуска — реакция")
	}
	if !again.isNew(edited(2, int(stop.Add(time.Hour).Unix())), restart) {
		t.Fatal("правка, сделанная пока служба не работала, должна прийти")
	}
	if again.isNew(edited(3, int(stop.Add(-48*time.Hour).Unix())), restart) {
		t.Fatal("давняя правка (до остановки) без известной даты — реакция")
	}
}
