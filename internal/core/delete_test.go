package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"tgagent/internal/bot"
	"tgagent/internal/config"
	"tgagent/internal/lock"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/tgc"
)

func deleteEnv(t *testing.T) *config.Settings {
	t.Helper()
	s := watchEnv(t)
	s.DraftTTLMin = 60
	s.AutoSendDelaySec = 30
	r := s.Chats["test"]
	r.Send = true
	s.Chats["test"] = r
	return s
}

func TestDeleteOnlyOwnSentMessages(t *testing.T) {
	s := deleteEnv(t)
	box := outbox.New(s.DataDir())
	d, err := box.Create(outbox.CreateOpts{Chat: "test", Text: "старый статус", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Approve(d.ID, "test"); err != nil {
		t.Fatal(err)
	}
	// Сообщение отправлено 3 дня назад — EditWindowMin на удаление не действует.
	oldSent := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := box.MarkSent(d.ID, 555, oldSent, outbox.SentMeta{PeerID: -1000000000077}); err != nil {
		t.Fatal(err)
	}

	if got, err := ownSentTarget(s, "test", 555, 0); err != nil || got.ID != d.ID {
		t.Fatalf("своё отправленное: got=%v err=%v", got, err)
	}

	// Сообщение владельца или чужое (нет sent-черновика) — строгий отказ.
	var dn *Denied
	if _, err := ownSentTarget(s, "test", 556, 0); !errors.As(err, &dn) || !strings.Contains(err.Error(), "не через tg-agent") {
		t.Fatalf("чужое/владельца сообщение: %v", err)
	}
	if _, err := DeleteMessage(context.Background(), s, "test", 556, "", "проверка"); !errors.As(err, &dn) || !strings.Contains(err.Error(), "не через tg-agent") {
		t.Fatalf("DeleteMessage для сообщения владельца не отказал Denied (не через tg-agent): %v", err)
	}
	if _, err := ownSentTarget(s, "other", 555, 0); !errors.As(err, &dn) {
		t.Fatalf("тот же id в другом чате не наш: %v", err)
	}

	// Старый черновик без PeerID (отправленный до обновления) отклоняется.
	legacy, err := box.Create(outbox.CreateOpts{Chat: "test", Text: "до обновления", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.MarkSent(legacy.ID, 559, oldSent); err != nil {
		t.Fatal(err)
	}
	if _, err := ownSentTarget(s, "test", 559, 0); !errors.As(err, &dn) || !strings.Contains(err.Error(), "идентификатор чата") {
		t.Fatalf("черновик без PeerID должен отклоняться: %v", err)
	}

	// Черновики Vote / Bot / EditOf / DeleteOf не считаются исходным отправленным сообщением.
	for _, opts := range []outbox.CreateOpts{
		{Chat: "test", TTLMin: 60, Vote: &outbox.VoteSpec{MessageID: 601, Options: []int{0}}},
		{Chat: "test", TTLMin: 60, Bot: &outbox.BotSpec{Action: "token", Bot: "@xbot"}},
		{Chat: "test", Text: "правка", TTLMin: 60, EditOf: 602, EditOld: "было"},
		{Chat: "test", TTLMin: 60, DeleteOf: 603, DeleteOld: "старое"},
	} {
		x, err := box.Create(opts)
		if err != nil {
			t.Fatal(err)
		}
		mid := int64(601)
		switch {
		case opts.Bot != nil:
			mid = 604
		case opts.EditOf != 0:
			mid = opts.EditOf
		case opts.DeleteOf != 0:
			mid = opts.DeleteOf
		}
		if _, err := box.MarkSent(x.ID, mid, oldSent, outbox.SentMeta{PeerID: -1000000000077}); err != nil {
			t.Fatal(err)
		}
		if _, err := ownSentTarget(s, "test", mid, 0); !errors.As(err, &dn) {
			t.Fatalf("черновик %+v ошибочно принят за исходное сообщение %d: %v", opts, mid, err)
		}
	}
}

func TestEditTargetIgnoresSentDeleteDraft(t *testing.T) {
	s := deleteEnv(t)
	box := outbox.New(s.DataDir())

	// Только sent-черновик удаления с message_id=777 — editTarget не должен считать его исходным.
	delOnly, err := box.Create(outbox.CreateOpts{Chat: "test", TTLMin: 60, DeleteOf: 777, DeleteOld: "удалённое"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.MarkSent(delOnly.ID, 777, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, err := editTarget(s, "test", 777); err == nil {
		t.Fatal("editTarget перепутал sent-черновик удаления с исходным сообщением")
	}
	var bad *Bad
	if _, err := EditMessage(context.Background(), s, "test", 0, delOnly.ID, "новый текст", "", "markdown"); !errors.As(err, &bad) {
		t.Fatalf("EditMessage по draft_id удаления должен вернуть Bad, получили: %v", err)
	}

	// Есть и исходный, и черновик удаления — editTarget возвращает именно исходный.
	orig, err := box.Create(outbox.CreateOpts{Chat: "test", Text: "исходный", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.MarkSent(orig.ID, 777, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	got, err := editTarget(s, "test", 777)
	if err != nil || got.ID != orig.ID {
		t.Fatalf("editTarget вернул %v (%v), ждали исходный %s", got, err, orig.ID)
	}

	// savedFileDraft тоже отклоняет DeleteOf.
	s.Chats["saved"] = config.ChatRule{Alias: "saved", Peer: "me", Read: true, Send: true}
	s.ChatOrder = append(s.ChatOrder, "saved")
	savedDel, err := box.Create(outbox.CreateOpts{
		Chat: "saved", TTLMin: 60, DeleteOf: 888, DeleteOld: "[audio]",
		Files: []outbox.FileSnap{{Path: "x.mp3", Name: "x.mp3", Size: 1, Kind: outbox.KindAudio}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.MarkSent(savedDel.ID, 888, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := savedFileDraft(s, savedDel.ID); err == nil {
		t.Fatal("savedFileDraft принял черновик удаления")
	}
}

func TestDeleteNonTelegramRejected(t *testing.T) {
	for _, net := range []string{config.NetMAX, config.NetWA} {
		t.Run(net, func(t *testing.T) {
			s := deleteEnv(t)
			s.Network = net
			var bad *Bad
			_, err := DeleteMessage(context.Background(), s, "test", 555, "", "")
			if !errors.As(err, &bad) {
				t.Fatalf("сеть %s: ждали Bad, получили %v", net, err)
			}

			// Защита и на уровне deliverClaimed.
			box := outbox.New(s.DataDir())
			d, err := box.Create(outbox.CreateOpts{Chat: "test", TTLMin: 60, DeleteOf: 555, DeleteOld: "x"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := box.Approve(d.ID, "test"); err != nil {
				t.Fatal(err)
			}
			_, err = deliverClaimed(context.Background(), s, d.ID, "test")
			if !errors.As(err, &bad) {
				t.Fatalf("deliverClaimed в %s: ждали Bad, получили %v", net, err)
			}
		})
	}
}

func TestDeleteContainerAgentVisibility(t *testing.T) {
	s := deleteEnv(t)
	s.Chats["secret"] = config.ChatRule{Alias: "secret", Peer: "@secret", Read: true, Send: true}
	s.ChatOrder = append(s.ChatOrder, "secret")

	box := outbox.New(s.DataDir())
	secretDraft, err := box.Create(outbox.CreateOpts{Chat: "secret", Text: "чужой чат", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.MarkSent(secretDraft.ID, 901, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	restricted := s.Restrict(&config.Agent{Name: "sandbox", Chats: []string{"test"}})

	var nf *outbox.NotFound
	if _, err := ownSentTarget(restricted, "secret", 901, 0); !errors.As(err, &nf) {
		t.Fatalf("ownSentTarget для чужого чата контейнера: ждали NotFound, получили %v", err)
	}
	if _, err := DeleteMessage(context.Background(), restricted, "test", 0, secretDraft.ID, ""); !errors.As(err, &nf) {
		t.Fatalf("DeleteMessage по draft_id чужого чата контейнера: ждали NotFound, получили %v", err)
	}
}

func TestInspectDeleteTargetChecksPeerAndOut(t *testing.T) {
	cases := []struct {
		name     string
		target   tgc.Target
		scenario string
		wantOld  string
		wantBad  bool
		wantDeny bool
		wantErr  bool
	}{
		{"own_text", tgc.Target{Kind: "user", ID: 10, Input: &tg.InputPeerUser{UserID: 10}}, "text", "Привет", false, false, false},
		{"own_photo_no_text", tgc.Target{Kind: "user", ID: 10, Input: &tg.InputPeerUser{UserID: 10}}, "photo", "[photo]", false, false, false},
		{"own_doc_no_text", tgc.Target{Kind: "user", ID: 10, Input: &tg.InputPeerUser{UserID: 10}}, "doc", "[file:report.pdf]", false, false, false},
		{"self_without_out", tgc.Target{Kind: "self", ID: 1, Input: &tg.InputPeerSelf{}}, "self", "заметка", false, false, false},
		{"incoming_not_out", tgc.Target{Kind: "user", ID: 10, Input: &tg.InputPeerUser{UserID: 10}}, "incoming", "", false, true, false},
		{"other_dialog", tgc.Target{Kind: "user", ID: 10, Input: &tg.InputPeerUser{UserID: 10}}, "other_dialog", "", true, false, false},
		{"missing", tgc.Target{Kind: "user", ID: 10, Input: &tg.InputPeerUser{UserID: 10}}, "missing", "", true, false, false},
		{"read_error", tgc.Target{Kind: "user", ID: 10, Input: &tg.InputPeerUser{UserID: 10}}, "read_error", "", false, false, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &tgc.Conn{Peers: tgc.LoadPeers("")}
			c.API = tg.NewClient(cleanupInvoker(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				if _, ok := in.(*tg.MessagesGetMessagesRequest); !ok {
					t.Fatalf("unexpected RPC %T", in)
				}
				if tc.scenario == "read_error" {
					return errors.New("synthetic read failure")
				}
				var msgs []tg.MessageClass
				switch tc.scenario {
				case "text":
					msgs = []tg.MessageClass{&tg.Message{ID: 123, Out: true, PeerID: &tg.PeerUser{UserID: 10}, Message: "Привет"}}
				case "photo":
					msgs = []tg.MessageClass{&tg.Message{
						ID: 123, Out: true, PeerID: &tg.PeerUser{UserID: 10},
						Media: &tg.MessageMediaPhoto{Photo: &tg.Photo{ID: 1}},
					}}
				case "doc":
					msgs = []tg.MessageClass{&tg.Message{
						ID: 123, Out: true, PeerID: &tg.PeerUser{UserID: 10},
						Media: &tg.MessageMediaDocument{Document: &tg.Document{
							ID:         2,
							Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "report.pdf"}},
						}},
					}}
				case "self":
					msgs = []tg.MessageClass{&tg.Message{ID: 123, Out: false, PeerID: &tg.PeerUser{UserID: 1}, Message: "заметка"}}
				case "incoming":
					msgs = []tg.MessageClass{&tg.Message{ID: 123, Out: false, PeerID: &tg.PeerUser{UserID: 10}, Message: "чужое"}}
				case "other_dialog":
					msgs = []tg.MessageClass{&tg.Message{ID: 123, Out: true, PeerID: &tg.PeerUser{UserID: 99}, Message: "в другом чате"}}
				case "missing":
					msgs = nil
				}
				out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{Messages: msgs}
				return nil
			}))

			got, err := inspectDeleteTarget(context.Background(), c, tc.target, 123)
			var bad *Bad
			var dn *Denied
			switch {
			case tc.wantBad:
				if !errors.As(err, &bad) {
					t.Fatalf("ждали Bad, получили %v", err)
				}
			case tc.wantDeny:
				if !errors.As(err, &dn) {
					t.Fatalf("ждали Denied, получили %v", err)
				}
			case tc.wantErr:
				if err == nil {
					t.Fatal("ждали ошибку, получили nil")
				}
			default:
				if err != nil || got != tc.wantOld {
					t.Fatalf("got=%q err=%v, ждали %q", got, err, tc.wantOld)
				}
			}
		})
	}
}

func TestDeleteMessageByMessageIDAndDraftID(t *testing.T) {
	s := deleteEnv(t)
	s.SendPolicy = "human_approval"
	box := outbox.New(s.DataDir())

	// Исходное отправленное сообщение (с темой форума) и его правка.
	orig, err := box.Create(outbox.CreateOpts{Chat: "test", Text: "версия 1", TTLMin: 60, Topic: 42, TopicTitle: "Деплой"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.MarkSent(orig.ID, 555, time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339), outbox.SentMeta{PeerID: -1000000000077}); err != nil {
		t.Fatal(err)
	}
	editD, err := box.Create(outbox.CreateOpts{Chat: "test", Text: "версия 2", TTLMin: 60, EditOf: 555, EditOld: "версия 1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.MarkSent(editD.ID, 555, time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339), outbox.SentMeta{PeerID: -1000000000077}); err != nil {
		t.Fatal(err)
	}

	prev := fetchDeleteOld
	t.Cleanup(func() { fetchDeleteOld = prev })
	var fetchedIDs []int64
	fetchDeleteOld = func(_ context.Context, _ *config.Settings, _ config.ChatRule, messageID, _ int64) (string, int64, error) {
		fetchedIDs = append(fetchedIDs, messageID)
		return "версия 2", -1000000000077, nil
	}

	// 1. По message_id.
	out1, err := DeleteMessage(context.Background(), s, "test", 555, "", "устарело")
	if err != nil {
		t.Fatalf("DeleteMessage by message_id: %v", err)
	}
	if oldText, ok := out1.Get("old_text"); !ok || oldText != "версия 2" {
		t.Fatalf("old_text при read=true: ok=%v val=%v", ok, oldText)
	}
	id1, _ := out1.Get("draft_id")
	d1, err := box.Get(id1.(string))
	if err != nil {
		t.Fatal(err)
	}
	if d1.DeleteOf != 555 || d1.DeleteOld != "версия 2" || d1.DeletePeer != -1000000000077 || d1.Topic != 42 || d1.TopicTitle != "Деплой" || d1.Status != outbox.Pending {
		t.Fatalf("черновик удаления: %+v", d1)
	}

	// 2. По исходному draft_id.
	out2, err := DeleteMessage(context.Background(), s, "test", 0, orig.ID, "убрать")
	if err != nil {
		t.Fatalf("DeleteMessage by orig draft_id: %v", err)
	}
	if delOf, _ := out2.Get("delete_of"); toInt64(delOf) != 555 {
		t.Fatalf("delete_of = %v, ждали 555", delOf)
	}

	// 3. По draft_id правки (через d.EditOf).
	out3, err := DeleteMessage(context.Background(), s, "test", 0, editD.ID, "убрать после правки")
	if err != nil {
		t.Fatalf("DeleteMessage by edit draft_id: %v", err)
	}
	if delOf, _ := out3.Get("delete_of"); toInt64(delOf) != 555 {
		t.Fatalf("delete_of по draft_id правки = %v, ждали 555", delOf)
	}
	if len(fetchedIDs) != 3 || fetchedIDs[0] != 555 || fetchedIDs[1] != 555 || fetchedIDs[2] != 555 {
		t.Fatalf("fetchDeleteOld вызван с %v", fetchedIDs)
	}

	// 4. По draft_id самого удаления — отказ Bad.
	if _, err := box.MarkSent(d1.ID, 555, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	var bad *Bad
	if _, err := DeleteMessage(context.Background(), s, "test", 0, d1.ID, ""); !errors.As(err, &bad) {
		t.Fatalf("DeleteMessage по draft_id удаления: ждали Bad, получили %v", err)
	}

	// 5. Auto-send чат → scheduled с окном отмены и правильным next_step.
	// Держим updates-lock, чтобы ScheduleLocal не запускал внешний процесс в тесте.
	ul := lock.New(s.UpdatesLockPath(), time.Second)
	if err := ul.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ul.Release)
	rule := s.Chats["test"]
	rule.Auto = true
	s.Chats["test"] = rule
	outAuto, err := DeleteMessage(context.Background(), s, "test", 555, "", "авто")
	if err != nil {
		t.Fatalf("DeleteMessage auto_send: %v", err)
	}
	if st, _ := outAuto.Get("status"); st != "scheduled" {
		t.Fatalf("status = %v, ждали scheduled", st)
	}
	if next, _ := outAuto.Get("next_step"); !strings.Contains(next.(string), "сообщение УДАЛИТСЯ САМО") {
		t.Fatalf("next_step: %v", next)
	}

	// 6. ListDrafts включает delete_of; sentSummary пишет «Удалено»; EditDraft запрещает править.
	list, err := ListDrafts(s, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := omap.Pretty(list); !strings.Contains(got, `"delete_of": 555`) {
		t.Fatalf("ListDrafts не содержит delete_of:\n%s", got)
	}
	if sum := sentSummary(s, d1, 555); !strings.Contains(sum, "**Удалено**") {
		t.Fatalf("sentSummary = %q", sum)
	}
	idAuto, _ := outAuto.Get("draft_id")
	if err := EditDraft(context.Background(), s, idAuto.(string), "текст", "owner"); !errors.As(err, &bad) {
		t.Fatalf("EditDraft для удаления: ждали Bad, получили %v", err)
	}

	// 7. Карточка бота для DeleteOf.
	cardMD := bot.CardMarkdown(d1, "Тест", "устарело", "")
	for _, want := range []string{"удалить своё сообщение", "#555", "> версия 2"} {
		if !strings.Contains(cardMD, want) {
			t.Fatalf("CardMarkdown без %q:\n%s", want, cardMD)
		}
	}
}

func TestSendDraftDeleteChecksPeerAndOutBeforeDeleting(t *testing.T) {
	cases := []struct {
		name      string
		target    tgc.Target
		scenario  string
		wantOK    bool
		wantDeny  bool
		wantNoSnt bool
	}{
		{"own_private", tgc.Target{Kind: "user", ID: 10, Input: &tg.InputPeerUser{UserID: 10}}, "own", true, false, false},
		{"self_without_out", tgc.Target{Kind: "self", ID: 1, Input: &tg.InputPeerSelf{}}, "self", true, false, false},
		{"own_channel", tgc.Target{Kind: "channel", ID: 77, Input: &tg.InputPeerChannel{ChannelID: 77, AccessHash: 9}}, "channel", true, false, false},
		{"other_dialog", tgc.Target{Kind: "user", ID: 10, Input: &tg.InputPeerUser{UserID: 10}}, "other_dialog", false, true, false},
		{"not_out", tgc.Target{Kind: "user", ID: 10, Input: &tg.InputPeerUser{UserID: 10}}, "not_out", false, true, false},
		{"missing", tgc.Target{Kind: "user", ID: 10, Input: &tg.InputPeerUser{UserID: 10}}, "missing", false, true, false},
		{"read_error", tgc.Target{Kind: "user", ID: 10, Input: &tg.InputPeerUser{UserID: 10}}, "read_error", false, false, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deletes := 0
			c := &tgc.Conn{Peers: tgc.LoadPeers("")}
			c.API = tg.NewClient(cleanupInvoker(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				switch req := in.(type) {
				case *tg.MessagesGetMessagesRequest:
					if tc.scenario == "read_error" {
						return errors.New("synthetic read error")
					}
					var msgs []tg.MessageClass
					switch tc.scenario {
					case "own":
						msgs = []tg.MessageClass{&tg.Message{ID: 123, Out: true, PeerID: &tg.PeerUser{UserID: 10}, Message: "ok"}}
					case "self":
						msgs = []tg.MessageClass{&tg.Message{ID: 123, Out: false, PeerID: &tg.PeerUser{UserID: 1}, Message: "ok"}}
					case "other_dialog":
						msgs = []tg.MessageClass{&tg.Message{ID: 123, Out: true, PeerID: &tg.PeerUser{UserID: 99}, Message: "other"}}
					case "not_out":
						msgs = []tg.MessageClass{&tg.Message{ID: 123, Out: false, PeerID: &tg.PeerUser{UserID: 10}, Message: "incoming"}}
					case "missing":
						msgs = nil
					}
					out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{Messages: msgs}
				case *tg.ChannelsGetMessagesRequest:
					out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesChannelMessages{
						Messages: []tg.MessageClass{&tg.Message{ID: 123, Out: true, PeerID: &tg.PeerChannel{ChannelID: 77}, Message: "ok"}},
					}
				case *tg.MessagesDeleteMessagesRequest:
					deletes++
					if !req.Revoke || len(req.ID) != 1 || req.ID[0] != 123 {
						t.Fatalf("unexpected delete req: %+v", req)
					}
					*out.(*tg.MessagesAffectedMessages) = tg.MessagesAffectedMessages{}
				case *tg.ChannelsDeleteMessagesRequest:
					deletes++
					if len(req.ID) != 1 || req.ID[0] != 123 {
						t.Fatalf("unexpected channel delete req: %+v", req)
					}
					*out.(*tg.MessagesAffectedMessages) = tg.MessagesAffectedMessages{}
				default:
					t.Fatalf("unexpected RPC %T", in)
				}
				return nil
			}))

			res, err := sendDraft(context.Background(), c, tc.target, &outbox.Draft{DeleteOf: 123, DeleteOld: "ok", DeletePeer: tc.target.MarkedID()})
			var dn *Denied
			var ns *NotSent
			switch {
			case tc.wantOK:
				if err != nil || deletes != 1 {
					t.Fatalf("err=%v deletes=%d", err, deletes)
				}
				mid, _ := res.Get("message_id")
				del, _ := res.Get("deleted")
				fmtVal, _ := res.Get("format")
				if toInt64(mid) != 123 || del != true || fmtVal != "delete" {
					t.Fatalf("unexpected result: %s", omap.Pretty(res))
				}
			case tc.wantDeny:
				if !errors.As(err, &dn) || deletes != 0 {
					t.Fatalf("ждали Denied и 0 удалений, получили err=%v deletes=%d", err, deletes)
				}
			case tc.wantNoSnt:
				if !errors.As(err, &ns) || deletes != 0 {
					t.Fatalf("ждали NotSent и 0 удалений, получили err=%v deletes=%d", err, deletes)
				}
			}
		})
	}
}

// P1 #1: переназначение alias на другой peer (например, между двумя супергруппами
// с совпадающими message_id) отклоняется и при создании черновика удаления, и при
// доставке (DeleteMessages не вызывается).
func TestDeletePeerMismatchRejected(t *testing.T) {
	t.Run("alias_rebound_before_delete_draft", func(t *testing.T) {
		s := deleteEnv(t)
		s.SendPolicy = "human_approval"
		box := outbox.New(s.DataDir())
		orig, err := box.Create(outbox.CreateOpts{Chat: "test", Text: "сообщение в супергруппе A", TTLMin: 60})
		if err != nil {
			t.Fatal(err)
		}
		const peerA = int64(-1000000000077)
		const peerB = int64(-1000000000088)
		if _, err := box.MarkSent(orig.ID, 42, time.Now().UTC().Format(time.RFC3339), outbox.SentMeta{PeerID: peerA}); err != nil {
			t.Fatal(err)
		}

		prev := fetchDeleteOld
		t.Cleanup(func() { fetchDeleteOld = prev })
		fetchDeleteOld = func(_ context.Context, _ *config.Settings, _ config.ChatRule, _, _ int64) (string, int64, error) {
			// alias теперь разрешается в супергруппу B
			return "чужое сообщение #42 в B", peerB, nil
		}

		var dn *Denied
		if _, err := DeleteMessage(context.Background(), s, "test", 42, "", ""); !errors.As(err, &dn) {
			t.Fatalf("ждали Denied при переназначении alias, получили %v", err)
		}
		if drafts, err := box.List(outbox.Pending); err != nil || len(drafts) != 0 {
			t.Fatalf("черновик удаления не должен был создаться: %+v err=%v", drafts, err)
		}
	})

	t.Run("alias_rebound_before_delivery", func(t *testing.T) {
		const peerA = int64(-1000000000077)
		targetB := tgc.Target{Kind: "channel", ID: 88, Input: &tg.InputPeerChannel{ChannelID: 88, AccessHash: 9}}
		rpcCalls := 0
		c := &tgc.Conn{Peers: tgc.LoadPeers("")}
		c.API = tg.NewClient(cleanupInvoker(func(_ context.Context, in bin.Encoder, _ bin.Decoder) error {
			rpcCalls++
			t.Fatalf("не ожидали RPC при несовпадении DeletePeer, пришёл %T", in)
			return nil
		}))

		d := &outbox.Draft{Chat: "test", DeleteOf: 42, DeleteOld: "ok", DeletePeer: peerA}
		_, err := sendDraft(context.Background(), c, targetB, d)
		var dn *Denied
		if !errors.As(err, &dn) || rpcCalls != 0 {
			t.Fatalf("ждали Denied и 0 RPC, получили err=%v rpcCalls=%d", err, rpcCalls)
		}
	})

	t.Run("sendDraft_and_finishDelivery_save_peer_and_scheduled", func(t *testing.T) {
		s := deleteEnv(t)
		box := outbox.New(s.DataDir())
		d, err := box.Create(outbox.CreateOpts{Chat: "test", Text: "в Избранное", TTLMin: 60})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := box.Approve(d.ID, "test"); err != nil {
			t.Fatal(err)
		}
		res, err := sendDraft(context.Background(), &fakeSender{}, tgc.Target{Kind: "self", ID: 9001, Input: &tg.InputPeerSelf{}}, d)
		if err != nil {
			t.Fatal(err)
		}
		if pid, _ := res.Get("peer_id"); toInt64(pid) != 9001 {
			t.Fatalf("peer_id = %v, ждали 9001", pid)
		}
		if sc, _ := res.Get("scheduled"); sc != true {
			t.Fatalf("scheduled = %v, ждали true", sc)
		}
		if _, err := finishDelivery(s, s.Chats["test"], d, d.ID, "test", res); err != nil {
			t.Fatal(err)
		}
		got, err := box.Get(d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.PeerID != 9001 || !got.Scheduled {
			t.Fatalf("finishDelivery не сохранил PeerID/Scheduled: %+v", got)
		}
	})
}

// P1 #2: черновик с Scheduled=true отклоняется в ownSentTarget и DeleteMessage,
// а после RelinkScheduled (который трогает только Scheduled=true) разрешается.
func TestDeleteScheduledDraftRejectedUntilRelinked(t *testing.T) {
	s := deleteEnv(t)
	s.Chats["saved"] = config.ChatRule{Alias: "saved", Peer: "me", Read: true, Send: true}
	s.ChatOrder = append(s.ChatOrder, "saved")
	box := outbox.New(s.DataDir())

	// Отложенное сообщение в Избранном (в MessageID пока id очереди = 94).
	sched, err := box.Create(outbox.CreateOpts{Chat: "saved", Text: "напоминание", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.MarkSent(sched.ID, 94, time.Now().UTC().Format(time.RFC3339), outbox.SentMeta{PeerID: 1, Scheduled: true}); err != nil {
		t.Fatal(err)
	}
	// Обычный черновик в другом чате с тем же числовым message_id=94 — RelinkScheduled не должен его тронуть.
	regular, err := box.Create(outbox.CreateOpts{Chat: "test", Text: "обычное", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.MarkSent(regular.ID, 94, time.Now().UTC().Format(time.RFC3339), outbox.SentMeta{PeerID: -1000000000077}); err != nil {
		t.Fatal(err)
	}

	var dn *Denied
	if _, err := ownSentTarget(s, "saved", 94, 0); !errors.As(err, &dn) || !strings.Contains(err.Error(), "отложенных") {
		t.Fatalf("ownSentTarget для Scheduled=true: ждали Denied, получили %v", err)
	}
	if _, err := DeleteMessage(context.Background(), s, "saved", 0, sched.ID, ""); !errors.As(err, &dn) || !strings.Contains(err.Error(), "отложенных") {
		t.Fatalf("DeleteMessage по draft_id для Scheduled=true: ждали Denied, получили %v", err)
	}

	relinked, err := box.RelinkScheduled(1, [][2]int64{{94, 3614944}})
	if err != nil {
		t.Fatal(err)
	}
	if len(relinked) != 1 || relinked[0].ID != sched.ID {
		t.Fatalf("RelinkScheduled перепривязал не то: %+v", relinked)
	}
	if gotReg, err := box.Get(regular.ID); err != nil || gotReg.MessageID == nil || *gotReg.MessageID != 94 {
		t.Fatalf("обычный черновик испорчен RelinkScheduled: %+v err=%v", gotReg, err)
	}

	// После доставки и перепривязки ownSentTarget принимает реальный id.
	got, err := ownSentTarget(s, "saved", 3614944, 0)
	if err != nil || got.ID != sched.ID || got.Scheduled {
		t.Fatalf("после RelinkScheduled: got=%+v err=%v", got, err)
	}
}

// P1 #3: эвристически восстановленный через repair.go черновик (Repaired=true)
// отклоняется для удаления.
func TestDeleteRepairedDraftRejected(t *testing.T) {
	s := deleteEnv(t)
	box := outbox.New(s.DataDir())

	d, err := box.Create(outbox.CreateOpts{Chat: "test", Text: "старое без id", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.MarkSent(d.ID, 0, time.Now().UTC().Format(time.RFC3339), outbox.SentMeta{PeerID: -1000000000077}); err != nil {
		t.Fatal(err)
	}
	setRepaired(s, box, d, 555, true)

	got, err := box.Get(d.ID)
	if err != nil || !got.Repaired || got.MessageID == nil || *got.MessageID != 555 {
		t.Fatalf("setRepaired не поставил Repaired=true: %+v err=%v", got, err)
	}

	var dn *Denied
	if _, err := ownSentTarget(s, "test", 555, 0); !errors.As(err, &dn) || !strings.Contains(err.Error(), "эвристически") {
		t.Fatalf("ownSentTarget для Repaired=true: ждали Denied, получили %v", err)
	}
	if _, err := DeleteMessage(context.Background(), s, "test", 0, d.ID, ""); !errors.As(err, &dn) || !strings.Contains(err.Error(), "эвристически") {
		t.Fatalf("DeleteMessage по draft_id для Repaired=true: ждали Denied, получили %v", err)
	}
}

// Регрессия: два черновика с одинаковым alias и message_id, но разными PeerID
// (после переназначения alias из A в B). Запрос по draft_id из A не должен
// молча подцепить черновик из B и удалить сообщение в B.
func TestDeleteByDraftIDDoesNotSwitchPeerOnSameMessageID(t *testing.T) {
	s := deleteEnv(t)
	box := outbox.New(s.DataDir())
	now := time.Now().UTC().Format(time.RFC3339)

	draftA, err := box.Create(outbox.CreateOpts{Chat: "test", Text: "в группе A", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.MarkSent(draftA.ID, 555, now, outbox.SentMeta{PeerID: -1000000000001}); err != nil {
		t.Fatal(err)
	}
	draftB, err := box.Create(outbox.CreateOpts{Chat: "test", Text: "в группе B", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.MarkSent(draftB.ID, 555, now, outbox.SentMeta{PeerID: -1000000000002}); err != nil {
		t.Fatal(err)
	}

	prev := fetchDeleteOld
	t.Cleanup(func() { fetchDeleteOld = prev })
	fetchDeleteOld = func(_ context.Context, _ *config.Settings, _ config.ChatRule, _, expectedPeer int64) (string, int64, error) {
		if expectedPeer != -1000000000001 {
			t.Fatalf("ждали expectedPeer группы A (-1000000000001), получили %d", expectedPeer)
		}
		return "", -1000000000002, denied("Alias теперь указывает на другой чат")
	}

	var dn *Denied
	if _, err := DeleteMessage(context.Background(), s, "test", 0, draftA.ID, ""); !errors.As(err, &dn) {
		t.Fatalf("удаление по draft_id из A после переезда alias в B: ждали Denied, получили %v", err)
	}
}

// P1 #4: в чате с send=true, read=false DeleteMessage не возвращает текст из
// Telegram в ответе агенту (нет old_text), но сохраняет DeleteOld в черновике
// для карточки владельца.
func TestDeleteWriteOnlyChatHidesOldTextFromAgent(t *testing.T) {
	s := deleteEnv(t)
	s.SendPolicy = "human_approval"
	r := s.Chats["test"]
	r.Read = false
	r.Send = true
	s.Chats["test"] = r

	box := outbox.New(s.DataDir())
	orig, err := box.Create(outbox.CreateOpts{Chat: "test", Text: "черновик", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.MarkSent(orig.ID, 555, time.Now().UTC().Format(time.RFC3339), outbox.SentMeta{PeerID: -1000000000077}); err != nil {
		t.Fatal(err)
	}

	prev := fetchDeleteOld
	t.Cleanup(func() { fetchDeleteOld = prev })
	fetchDeleteOld = func(_ context.Context, _ *config.Settings, _ config.ChatRule, _, _ int64) (string, int64, error) {
		return "секретный текст из Telegram", -1000000000077, nil
	}

	out, err := DeleteMessage(context.Background(), s, "test", 555, "", "убрать")
	if err != nil {
		t.Fatalf("DeleteMessage при read=false: %v", err)
	}
	if v, ok := out.Get("old_text"); ok {
		t.Fatalf("при read=false old_text не должен попадать в ответ агенту, получили %v", v)
	}
	if pretty := omap.Pretty(out); strings.Contains(pretty, "секретный текст") {
		t.Fatalf("текст из Telegram утёк в ответ агенту:\n%s", pretty)
	}
	id, _ := out.Get("draft_id")
	d, err := box.Get(id.(string))
	if err != nil {
		t.Fatal(err)
	}
	if d.DeleteOld != "секретный текст из Telegram" || d.DeletePeer != -1000000000077 {
		t.Fatalf("в черновике для владельца должен сохраниться DeleteOld и DeletePeer: %+v", d)
	}
	if cardMD := bot.CardMarkdown(d, "Тест", "убрать", ""); !strings.Contains(cardMD, "> секретный текст из Telegram") {
		t.Fatalf("в карточке владельца нет цитаты:\n%s", cardMD)
	}
}
