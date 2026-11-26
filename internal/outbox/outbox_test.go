package outbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tgagent/internal/store"
)

func TestExtraFieldsKeptAndScheduled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "outbox.json") // установка до переезда на state.db
	os.WriteFile(path, []byte(`[{"future_field": {"x": 1}, "id": "a", "chat": "c", "text": "t", "reply_to": null,
	  "created_at": "2026-01-01T00:00:00+00:00", "expires_at": "2099-01-01T00:00:00+00:00", "status": "pending",
	  "files": [], "history": []}]`), 0o600)
	o := New(dir)
	if _, err := o.Approve("a", "x"); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := db.Q().QueryRow("SELECT body FROM drafts WHERE id = 'a'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	json.Unmarshal([]byte(raw), &v)
	if v["future_field"] == nil || v["status"] != "approved" {
		t.Fatalf("потеряно поле или статус: %s", raw)
	}
	at := time.Now().Add(-time.Second)
	d, err := o.Create(CreateOpts{Chat: "c", Text: "hi", TTLMin: 60, SendAt: &at})
	if err != nil || d.Status != Scheduled {
		t.Fatal(err, d.Status)
	}
	due, err := o.ClaimDue(time.Now())
	if err != nil || len(due) != 1 || due[0].ID != d.ID || due[0].ApprovedBy() != "auto_send" {
		t.Fatal("ClaimDue", err, len(due))
	}
	due, _ = o.ClaimDue(time.Now())
	if len(due) != 0 {
		t.Fatal("повторный захват")
	}
}

// «Отправить сейчас» у автоотправки: одобрение до срока, таймер его не трогает.
func TestApproveScheduledBeforeDue(t *testing.T) {
	o := New(t.TempDir())
	at := time.Now().Add(-time.Second)
	d, err := o.Create(CreateOpts{Chat: "c", Text: "hi", TTLMin: 60, SendAt: &at})
	if err != nil {
		t.Fatal(err)
	}
	got, err := o.Approve(d.ID, "telegram_button:1")
	if err != nil || got.Status != Approved || got.ApprovedBy() != "telegram_button:1" {
		t.Fatalf("%v %+v", err, got)
	}
	if due, _ := o.ClaimDue(time.Now()); len(due) != 0 {
		t.Fatal("таймер подхватил уже одобренный черновик")
	}
	if _, err := o.Approve(d.ID, "x"); err == nil {
		t.Fatal("повторное одобрение прошло")
	}
}

func TestDeleteDraftJSONRoundTrip(t *testing.T) {
	o := New(t.TempDir())
	d, err := o.Create(CreateOpts{Chat: "c", TTLMin: 60, DeleteOf: 4321, DeleteOld: "старый текст", DeletePeer: -1000000000077})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["delete_of"] != float64(4321) || m["delete_old"] != "старый текст" || m["delete_peer"] != float64(-1000000000077) {
		t.Fatalf("json fields missing: %s", string(raw))
	}
	got, err := o.Get(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DeleteOf != 4321 || got.DeleteOld != "старый текст" || got.DeletePeer != -1000000000077 {
		t.Fatalf("round-trip failed: %+v", got)
	}

	// MarkSent с SentMeta (PeerID, Scheduled) и SetRepairedMessageID сохраняются в JSON и SQLite.
	sentD, err := o.Create(CreateOpts{Chat: "saved", Text: "привет", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	sentD, err = o.MarkSent(sentD.ID, 94, time.Now().UTC().Format(time.RFC3339), SentMeta{PeerID: 12345, Scheduled: true})
	if err != nil {
		t.Fatal(err)
	}
	if sentD.PeerID != 12345 || !sentD.Scheduled {
		t.Fatalf("MarkSent meta lost: %+v", sentD)
	}
	gotSent, err := o.Get(sentD.ID)
	if err != nil || gotSent.PeerID != 12345 || !gotSent.Scheduled || gotSent.Repaired {
		t.Fatalf("Get after MarkSent meta: %+v err=%v", gotSent, err)
	}

	repD, err := o.Create(CreateOpts{Chat: "c", Text: "без id", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.MarkSent(repD.ID, 0, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, err := o.SetRepairedMessageID(repD.ID, 555, true); err != nil {
		t.Fatal(err)
	}
	gotRep, err := o.Get(repD.ID)
	if err != nil || gotRep.MessageID == nil || *gotRep.MessageID != 555 || !gotRep.Repaired {
		t.Fatalf("Get after SetRepairedMessageID: %+v err=%v", gotRep, err)
	}

	// Обычный черновик не содержит delete_of / delete_old / delete_peer / peer_id / scheduled / repaired в JSON.
	plain, err := o.Create(CreateOpts{Chat: "c", Text: "привет", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	plainRaw, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	var pm map[string]any
	if err := json.Unmarshal(plainRaw, &pm); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"delete_of", "delete_old", "delete_peer", "peer_id", "scheduled", "repaired"} {
		if _, ok := pm[k]; ok {
			t.Fatalf("unexpected %s in plain draft: %s", k, string(plainRaw))
		}
	}
}
