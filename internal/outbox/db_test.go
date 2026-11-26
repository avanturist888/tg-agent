package outbox

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tgagent/internal/store"
)

func approvedDraft(t *testing.T, o *Outbox) *Draft {
	t.Helper()
	d, err := o.Create(CreateOpts{Chat: "c", Text: "hi", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if d, err = o.Approve(d.ID, "test"); err != nil {
		t.Fatal(err)
	}
	return d
}

// Захват — ровно у одного из многих одновременных путей.
func TestClaimSendOnce(t *testing.T) {
	o := New(t.TempDir())
	d := approvedDraft(t, o)
	var (
		won  atomic.Int32
		busy atomic.Int32
		wg   sync.WaitGroup
		mu   sync.Mutex
		held []*Claim
	)
	for range 10 {
		wg.Go(func() {
			c, _, err := o.ClaimSend(d.ID)
			switch {
			case errors.Is(err, ErrSending):
				busy.Add(1)
			case err != nil:
				t.Error(err)
			case c != nil:
				won.Add(1)
				mu.Lock()
				held = append(held, c)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if won.Load() != 1 || busy.Load() != 9 {
		t.Fatalf("won %d busy %d", won.Load(), busy.Load())
	}
	held[0].Release()
	c, _, err := o.ClaimSend(d.ID)
	if c == nil || err != nil {
		t.Fatalf("после Release: %v", err)
	}
	c.Release()
}

// Не approved — отправлять нечего: захвата нет, статус — в черновике.
func TestClaimSendNotApproved(t *testing.T) {
	o := New(t.TempDir())
	d, err := o.Create(CreateOpts{Chat: "c", Text: "hi", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	c, cur, err := o.ClaimSend(d.ID)
	if c != nil || err != nil || cur.Status != Pending {
		t.Fatalf("%v %v %+v", c, err, cur)
	}
	if _, _, err := o.ClaimSend("nope"); err == nil {
		t.Fatal("нет черновика — нет ошибки")
	}
}

func setSender(t *testing.T, o *Outbox, id string, pid int, since float64) {
	t.Helper()
	db, err := store.Open(o.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE drafts SET sender_pid = ?, sender_since = ? WHERE id = ?", pid, since, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// Отметка живого другого процесса держит; умершего — нет (досылка после
// падения берёт черновик сразу, без ожиданий и возраста).
func TestClaimSendOtherProcess(t *testing.T) {
	o := New(t.TempDir())
	d := approvedDraft(t, o)
	alive := true
	was := pidAlive
	pidAlive = func(pid int, since float64) bool { return pid == 4242 && alive }
	t.Cleanup(func() { pidAlive = was })
	setSender(t, o, d.ID, 4242, float64(time.Now().Unix()))
	if _, _, err := o.ClaimSend(d.ID); !errors.Is(err, ErrSending) {
		t.Fatalf("живой чужой захват: %v", err)
	}
	alive = false
	c, _, err := o.ClaimSend(d.ID)
	if c == nil || err != nil {
		t.Fatalf("захват умершего не снят: %v", err)
	}
	c.Release()
}

// Своя отметка, которую не удалось снять (её нет в claims), не держит.
func TestClaimSendOwnOrphan(t *testing.T) {
	o := New(t.TempDir())
	d := approvedDraft(t, o)
	setSender(t, o, d.ID, os.Getpid(), 1)
	c, _, err := o.ClaimSend(d.ID)
	if c == nil || err != nil {
		t.Fatalf("свой осиротевший захват держит: %v", err)
	}
	c.Release()
}

// Создание не выбрасывает нерешённые черновики, сколько бы ни было решённых.
func TestCreateKeepsUndecided(t *testing.T) {
	o := New(t.TempDir())
	first, err := o.Create(CreateOpts{Chat: "c", Text: "ждёт", TTLMin: 600})
	if err != nil {
		t.Fatal(err)
	}
	for range keep + 5 {
		d, err := o.Create(CreateOpts{Chat: "c", Text: "x", TTLMin: 60})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := o.Cancel(d.ID, "t"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := o.Get(first.ID); err != nil {
		t.Fatalf("нерешённый выброшен: %v", err)
	}
	all, _ := o.List("")
	if len(all) > keep+2 {
		t.Fatalf("решённых не убрано: %d", len(all))
	}
}

// Протухание видно сразу и записывается с ближайшим изменением.
func TestExpiry(t *testing.T) {
	o := New(t.TempDir())
	d, err := o.Create(CreateOpts{Chat: "c", Text: "x", TTLMin: 0})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if got, _ := o.Get(d.ID); got.Status != Expired {
		t.Fatalf("статус %s", got.Status)
	}
	if _, err := o.Approve(d.ID, "t"); err == nil {
		t.Fatal("протухший одобрен")
	}
}

// После tg rollback (отметка в data/) эта сборка ничего не берёт в отправку.
func TestClaimSendRefusedAfterRollback(t *testing.T) {
	dir := t.TempDir()
	o := New(dir)
	d := approvedDraft(t, o)
	if err := os.WriteFile(filepath.Join(dir, store.RollbackFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, _, err := o.ClaimSend(d.ID); c != nil || !errors.Is(err, ErrRolledBack) {
		t.Fatalf("захват после отката: %v %v", c, err)
	}
}

// Уборка старых решённых не выбрасывает черновики, чей итог ещё не показан в
// карточке: ушедший с CardDue и с ждущим итогом в card_states.
func TestPruneKeepsUnshownCards(t *testing.T) {
	dir := t.TempDir()
	o := New(dir)
	bm := int64(7)
	sent := func() *Draft {
		d := approvedDraft(t, o)
		if _, err := o.Update(d.ID, func(d *Draft) error { d.BotMessageID = &bm; return nil }); err != nil {
			t.Fatal(err)
		}
		d, err := o.MarkSent(d.ID, 1, time.Now().Format(time.RFC3339))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	due := sent()
	if !due.CardDue {
		t.Fatal("MarkSent не поставил CardDue")
	}
	queued := sent()
	if err := o.CardShownIf(queued.ID, Sent, queued.CardDueAt); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO "+store.TCardStates+"(key, body) VALUES(?, ?)", queued.ID, `{"card": 7, "status": "✅ ушло", "ver": 1, "shown": false}`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	gone := sent()
	if err := o.CardShownIf(gone.ID, Sent, gone.CardDueAt); err != nil {
		t.Fatal(err)
	}
	for range keep + 5 {
		d, err := o.Create(CreateOpts{Chat: "c", Text: "x", TTLMin: 60})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := o.Cancel(d.ID, "t"); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{due.ID, queued.ID} {
		if _, err := o.Get(id); err != nil {
			t.Fatalf("черновик с непоказанным итогом выброшен: %v", err)
		}
	}
	if _, err := o.Get(gone.ID); err == nil {
		t.Fatal("показанный итог не убран — проверка ничего не проверяет")
	}
}

// Уборка не выбрасывает черновик с квитанцией ушедшего (sent-unsaved): без
// него служба не допишет отметку отправки.
func TestPruneKeepsUnsavedReceipt(t *testing.T) {
	dir := t.TempDir()
	o := New(dir)
	d, err := o.Create(CreateOpts{Chat: "c", Text: "ушло", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Cancel(d.ID, "t"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, store.UnsavedDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, store.UnsavedDir, d.ID), []byte(`{"message_id": 5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for range keep + 5 {
		x, err := o.Create(CreateOpts{Chat: "c", Text: "x", TTLMin: 60})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := o.Cancel(x.ID, "t"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := o.Get(d.ID); err != nil {
		t.Fatalf("черновик с квитанцией выброшен: %v", err)
	}
}

// Черновик, который сейчас отправляет живой путь, не отменяется: сообщение
// всё равно уйдёт, а «отменено» было бы неправдой.
func TestCancelRefusedWhileSending(t *testing.T) {
	o := New(t.TempDir())
	d := approvedDraft(t, o)
	c, _, err := o.ClaimSend(d.ID)
	if err != nil || c == nil {
		t.Fatalf("захват: %v", err)
	}
	if _, err := o.Cancel(d.ID, "t"); !errors.Is(err, ErrSending) {
		t.Fatalf("отмена посреди отправки: %v", err)
	}
	c.Release()
	if _, err := o.Cancel(d.ID, "t"); err != nil {
		t.Fatalf("отмена после отпуска: %v", err)
	}
}

// Карточка привязалась, когда черновик уже ушёл (слали её дольше окна
// автоотправки): итог ждёт показа.
func TestAttachCardAfterSent(t *testing.T) {
	o := New(t.TempDir())
	d := approvedDraft(t, o)
	if _, err := o.MarkSent(d.ID, 1, time.Now().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	got, err := o.AttachCard(d.ID, 55)
	if err != nil || !got.CardDue {
		t.Fatalf("итог не ждёт показа: %v %v", err, got != nil && got.CardDue)
	}
}

// Пока перенос старых файлов не закончен, ничего не берётся в отправку:
// черновик из базы мог уже уйти или быть отменён старой сборкой.
func TestClaimWaitsLegacy(t *testing.T) {
	dir := t.TempDir()
	o := New(dir)
	d := approvedDraft(t, o)
	store.RequireCutover = true
	t.Cleanup(func() { store.RequireCutover = false })
	if err := os.WriteFile(filepath.Join(dir, "outbox.json"), []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ImportLegacy(); err != nil {
		t.Fatal(err)
	}
	if c, _, err := o.ClaimSend(d.ID); c != nil || !errors.Is(err, ErrLegacyPending) {
		t.Fatalf("захват при ждущем переносе: %v %v", c, err)
	}
	if due, err := o.ClaimDue(time.Now().Add(time.Hour)); err != nil || len(due) != 0 {
		t.Fatalf("автоотправка при ждущем переносе: %v %v", due, err)
	}
}

// Отложенная отправка ушла: черновикам sent с Scheduled=true и id очереди
// ставится реальный id доставленного сообщения, а флаг Scheduled сбрасывается.
// Обычный (не отложенный) черновик с таким же числовым id не трогается.
func TestRelinkScheduled(t *testing.T) {
	o := New(t.TempDir())
	d := approvedDraft(t, o)
	if _, err := o.MarkSent(d.ID, 1000, time.Now().Format(time.RFC3339), SentMeta{PeerID: 77, Scheduled: true}); err != nil {
		t.Fatal(err)
	}
	plainSameID := approvedDraft(t, o)
	if _, err := o.MarkSent(plainSameID.ID, 1000, time.Now().Format(time.RFC3339), SentMeta{PeerID: -1000000000099}); err != nil {
		t.Fatal(err)
	}
	other := approvedDraft(t, o)
	if _, err := o.MarkSent(other.ID, 2000, time.Now().Format(time.RFC3339), SentMeta{PeerID: 77, Scheduled: true}); err != nil {
		t.Fatal(err)
	}
	otherPeer := approvedDraft(t, o)
	if _, err := o.MarkSent(otherPeer.ID, 1000, time.Now().Format(time.RFC3339), SentMeta{PeerID: 88, Scheduled: true}); err != nil {
		t.Fatal(err)
	}
	// Другой peer не должен тронуть черновик peer=77.
	if relinked, err := o.RelinkScheduled(999, [][2]int64{{1000, 3000}}); err != nil || len(relinked) != 0 {
		t.Fatalf("чужой peer перепривязал: %v %v", relinked, err)
	}
	relinked, err := o.RelinkScheduled(77, [][2]int64{{1000, 3000}, {9999, 4444}})
	if err != nil {
		t.Fatal(err)
	}
	if len(relinked) != 1 || relinked[0].ID != d.ID || relinked[0].Queue != 1000 || relinked[0].Real != 3000 {
		t.Fatalf("relinked = %+v", relinked)
	}
	got, err := o.Get(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MessageID == nil || *got.MessageID != 3000 || got.Scheduled || got.PeerID != 77 {
		t.Fatalf("got = %+v, ждали message_id=3000 Scheduled=false PeerID=77", got)
	}
	if got, _ := o.Get(plainSameID.ID); got.MessageID == nil || *got.MessageID != 1000 || got.Scheduled {
		t.Fatalf("неотложенный черновик с тем же id переписан: %+v", got)
	}
	if got, _ := o.Get(other.ID); got.MessageID == nil || *got.MessageID != 2000 || !got.Scheduled {
		t.Fatalf("чужой черновик переписан: %+v", got)
	}
	if got, _ := o.Get(otherPeer.ID); got.MessageID == nil || *got.MessageID != 1000 || !got.Scheduled {
		t.Fatalf("отложенный черновик другого peer переписан: %+v", got)
	}
	if relinked, err := o.RelinkScheduled(77, [][2]int64{{1000, 3000}}); err != nil || len(relinked) != 0 {
		t.Fatalf("повторное связывание: %v %v", relinked, err)
	}
}