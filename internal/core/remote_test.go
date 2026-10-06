package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/t3/t3test"
)

// remoteEnvSetup — белый список из двух чатов, окружение «box» на поддельном
// T3 и агент из контейнера, которому открыт только чат test.
func remoteEnvSetup(t *testing.T) (*config.Settings, *config.Settings, *t3test.Fake) {
	t.Helper()
	s := watchEnv(t)
	s.Chats["other"] = config.ChatRule{Alias: "other", Peer: int64(-100456), Read: true, Send: true}
	s.ChatOrder = append(s.ChatOrder, "other")
	rule := s.Chats["test"]
	rule.Send = true
	s.Chats["test"] = rule
	s.SendPolicy, s.DraftTTLMin = "human_approval", 60

	f := t3test.New(t)
	f.AddToken("owner-token")
	tok := filepath.Join(t.TempDir(), "tok.json")
	os.WriteFile(tok, []byte(`{"token":"owner-token"}`), 0o600)
	os.MkdirAll(filepath.Join(config.Root, "config"), 0o700)
	cfg := fmt.Sprintf("[[env]]\nname = \"box\"\norigin = %q\nmodel = \"claude-sonnet-5-5\"\nproject = \"/workspace\"\n"+
		"token_file = %q\npoll_sec = 0.01\nturn_timeout_sec = 5\n", f.URL, filepath.ToSlash(tok))
	if err := os.WriteFile(config.T3Path(), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	remoteRun.Lock()
	clear(remoteRun.busy)
	clear(remoteRun.retry)
	clear(remoteRun.failing)
	remoteRun.Unlock()
	agent := s.Restrict(&config.Agent{Name: "box-agent", Chats: []string{"test"}, Env: "box"})
	return s, agent, f
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("не дождались: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func idle(key string) func() bool {
	return func() bool {
		remoteRun.Lock()
		defer remoteRun.Unlock()
		return !remoteRun.busy[key]
	}
}

func remoteSubOf(s *config.Settings, key string) *remoteSub {
	return loadRemote(remotePath(s))[key]
}

func turnText(cmd map[string]any) string {
	msg, _ := cmd["message"].(map[string]any)
	text, _ := msg["text"].(string)
	return text
}

func TestRemoteSubscribeWakesGatewayThread(t *testing.T) {
	s, agent, f := remoteEnvSetup(t)
	ctx := context.Background()
	feed := FeedPath(s, "test")
	appendNewer(feed, []int{10})
	key := remoteKey("box", "test")

	out, err := Subscribe(ctx, agent, "test", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if env, _ := out.Get("env"); env != "box" {
		t.Fatalf("подписка агента из контейнера — на его окружение: %v", out)
	}
	notifyRemote(ctx, s)
	if len(f.Commands("")) != 0 {
		t.Fatal("подписка с текущего момента: старое не будит")
	}

	// новые сообщения — тред заводится и будится
	appendNewer(feed, []int{11, 12})
	notifyRemote(ctx, s)
	waitUntil(t, "первый ход", func() bool { return len(f.Commands("thread.turn.start")) == 1 })
	waitUntil(t, "конец первого хода", idle(key))
	create := f.Last("thread.create")
	if create["title"] != "TG: test" || create["runtimeMode"] != "full-access" {
		t.Fatalf("thread.create: %v", create)
	}
	thread, _ := create["threadId"].(string)
	first := f.Last("thread.turn.start")
	if first["threadId"] != thread {
		t.Fatal("ход начат не в треде шлюза")
	}
	if text := turnText(first); !strings.Contains(text, "этот тред завела служба") ||
		!strings.Contains(text, "новых: 2") || !strings.Contains(text, "after_id=10") {
		t.Fatalf("текст первого хода:\n%s", text)
	}
	sub := remoteSubOf(s, key)
	if sub == nil || sub.Thread != thread || sub.After != 12 || sub.Turn == nil || sub.Turn.State != "completed" {
		t.Fatalf("подписка после хода: %+v", sub)
	}

	// второе сообщение — тот же тред, без вступления
	appendNewer(feed, []int{13})
	notifyRemote(ctx, s)
	waitUntil(t, "второй ход", func() bool { return len(f.Commands("thread.turn.start")) == 2 })
	waitUntil(t, "конец второго хода", idle(key))
	second := f.Last("thread.turn.start")
	if second["threadId"] != thread || len(f.Commands("thread.create")) != 1 {
		t.Fatal("второе уведомление — в тот же тред")
	}
	if text := turnText(second); strings.Contains(text, "этот тред завела") || !strings.Contains(text, "after_id=12") {
		t.Fatalf("текст второго хода:\n%s", text)
	}

	// свои отправки не будят, но отметку сдвигают
	markOutgoing("test", 14)
	appendNewer(feed, []int{14})
	notifyRemote(ctx, s)
	time.Sleep(50 * time.Millisecond)
	if len(f.Commands("thread.turn.start")) != 2 || remoteSubOf(s, key).After != 14 {
		t.Fatal("своё сообщение не должно будить тред")
	}

	// правка уже известного сообщения будит тред и после хода забывается
	noteChange(s, "test", []int{12}, false)
	noteChange(s, "test", []int{13}, true)
	notifyRemote(ctx, s)
	waitUntil(t, "ход о правке", func() bool { return len(f.Commands("thread.turn.start")) == 3 })
	waitUntil(t, "конец хода о правке", idle(key))
	if text := turnText(f.Last("thread.turn.start")); !strings.Contains(text, "изменены сообщения 12") ||
		!strings.Contains(text, "удалены сообщения 13") || strings.Contains(text, "новых:") {
		t.Fatalf("текст хода о правке:\n%s", text)
	}
	if sub := remoteSubOf(s, key); len(sub.Edited)+len(sub.Deleted) != 0 {
		t.Fatalf("правки после хода: %+v", sub)
	}

	// аудит знает имя агента
	raw, _ := os.ReadFile(s.AuditPath())
	if !strings.Contains(string(raw), `"agent_subscribed"`) || !strings.Contains(string(raw), `"box-agent"`) ||
		!strings.Contains(string(raw), `"agent_t3_turn"`) {
		t.Fatalf("аудит:\n%s", raw)
	}
}

func TestRemoteThreadDeletedIsRecreated(t *testing.T) {
	s, agent, f := remoteEnvSetup(t)
	ctx := context.Background()
	feed := FeedPath(s, "test")
	key := remoteKey("box", "test")
	if _, err := Subscribe(ctx, agent, "test", 0, 0); err != nil {
		t.Fatal(err)
	}
	appendNewer(feed, []int{1})
	notifyRemote(ctx, s)
	waitUntil(t, "первый ход", func() bool { return len(f.Commands("thread.turn.start")) == 1 })
	waitUntil(t, "конец хода", idle(key))
	old := remoteSubOf(s, key).Thread

	f.Delete(old) // владелец удалил тред в T3
	appendNewer(feed, []int{2})
	notifyRemote(ctx, s)
	waitUntil(t, "ход в новом треде", func() bool { return len(f.Commands("thread.turn.start")) == 2 })
	waitUntil(t, "конец хода", idle(key))
	sub := remoteSubOf(s, key)
	if sub.Thread == old || sub.Thread == "" || len(f.Commands("thread.create")) != 2 {
		t.Fatalf("удалённый тред: ждали новый, а тред %q (был %q)", sub.Thread, old)
	}
	if text := turnText(f.Last("thread.turn.start")); !strings.Contains(text, "этот тред завела") || !strings.Contains(text, "after_id=1") {
		t.Fatalf("в новом треде — вступление и уведомление:\n%s", text)
	}
}

func TestRemoteWaitsForRunningTurn(t *testing.T) {
	s, agent, f := remoteEnvSetup(t)
	f.FinishAfter = 0 // ход идёт, пока тест его не закончит
	ctx := context.Background()
	feed := FeedPath(s, "test")
	key := remoteKey("box", "test")
	if _, err := Subscribe(ctx, agent, "test", 0, 0); err != nil {
		t.Fatal(err)
	}
	appendNewer(feed, []int{5})
	notifyRemote(ctx, s)
	waitUntil(t, "первый ход", func() bool { return len(f.Commands("thread.turn.start")) == 1 })
	thread := remoteSubOf(s, key).Thread

	// ход ещё идёт — новое сообщение копится
	appendNewer(feed, []int{6})
	notifyRemote(ctx, s)
	time.Sleep(100 * time.Millisecond)
	if len(f.Commands("thread.turn.start")) != 1 {
		t.Fatal("пока идёт ход, новый не начинаем")
	}
	f.Finish(thread)
	waitUntil(t, "конец первого хода", idle(key))
	notifyRemote(ctx, s) // служба проснулась бы сама (pokeSubs)
	waitUntil(t, "второй ход", func() bool { return len(f.Commands("thread.turn.start")) == 2 })
	if text := turnText(f.Last("thread.turn.start")); !strings.Contains(text, "новых: 1") || !strings.Contains(text, "after_id=5") {
		t.Fatalf("накопленное уведомление:\n%s", text)
	}
	f.Finish(thread)
	waitUntil(t, "конец второго хода", idle(key))
}

func TestRemoteUnsubscribeKeepsThread(t *testing.T) {
	s, agent, f := remoteEnvSetup(t)
	ctx := context.Background()
	feed := FeedPath(s, "test")
	key := remoteKey("box", "test")
	if _, err := Subscribe(ctx, agent, "test", 0, 0); err != nil {
		t.Fatal(err)
	}
	appendNewer(feed, []int{1})
	notifyRemote(ctx, s)
	waitUntil(t, "ход", func() bool { return len(f.Commands("thread.turn.start")) == 1 })
	waitUntil(t, "конец хода", idle(key))
	thread := remoteSubOf(s, key).Thread

	if _, err := Unsubscribe(ctx, agent, "test"); err != nil {
		t.Fatal(err)
	}
	appendNewer(feed, []int{2})
	notifyRemote(ctx, s)
	time.Sleep(50 * time.Millisecond)
	if len(f.Commands("thread.turn.start")) != 1 {
		t.Fatal("после отписки тред не будим")
	}
	// снова подписались — тот же тред
	if out, err := Subscribe(ctx, agent, "test", 0, 0); err != nil {
		t.Fatal(err)
	} else if got, _ := out.Get("thread"); got != thread {
		t.Fatalf("тред шлюза должен сохраниться: %v", out)
	}
}

func TestRemoteAgentScope(t *testing.T) {
	s, agent, _ := remoteEnvSetup(t)
	ctx := context.Background()

	// чужой чат из белого списка агенту не виден
	if _, err := Subscribe(ctx, agent, "other", 0, 0); err == nil {
		t.Fatal("подписка на чат вне списка агента")
	}
	if _, err := ReadChat(ctx, agent, "other", ReadOpts{}); err == nil {
		t.Fatal("чтение чата вне списка агента")
	}
	list, err := ListChats(ctx, agent, false)
	if err != nil {
		t.Fatal(err)
	}
	if text := omap.Pretty(list); strings.Contains(text, "other") || strings.Contains(text, "path") {
		t.Fatalf("список чатов агента: %s", text)
	}

	// файлы с диска шлюза и скачивание на него — не для агента из контейнера
	if _, err := DraftMessage(ctx, agent, "test", "x", nil, "", "", []string{"C:/secret.txt"}, false, 0); err == nil ||
		!strings.Contains(err.Error(), "files недоступны") {
		t.Fatalf("файлы от агента из контейнера: %v", err)
	}
	if _, err := DownloadFile(ctx, agent, "test", 1); err == nil {
		t.Fatal("скачивание на диск шлюза")
	}

	// черновики чужих чатов агенту не видны
	box := outbox.New(s.OutboxPath())
	mine, err := DraftMessage(ctx, agent, "test", "привет", nil, "", "", nil, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := box.Create(outbox.CreateOpts{Chat: "other", Text: "чужое", TTLMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	drafts, err := ListDrafts(agent, "")
	if err != nil {
		t.Fatal(err)
	}
	if text := omap.Pretty(drafts); strings.Contains(text, foreign.ID) || !strings.Contains(text, fmt.Sprint(must(mine.Get("draft_id")))) {
		t.Fatalf("черновики агента: %s", text)
	}
	var nf *outbox.NotFound
	if _, err := WaitApproval(ctx, agent, foreign.ID, 1); !errors.As(err, &nf) {
		t.Fatalf("ожидание чужого черновика: %v", err)
	}
	if _, err := SendDraft(ctx, agent, foreign.ID, ""); !errors.As(err, &nf) {
		t.Fatalf("отправка чужого черновика: %v", err)
	}
	if _, err := CancelDraft(ctx, agent, foreign.ID, "agent"); !errors.As(err, &nf) {
		t.Fatalf("отмена чужого черновика: %v", err)
	}
	if d, _ := box.Get(foreign.ID); d.Status != outbox.Pending {
		t.Fatal("чужой черновик не должен меняться")
	}
	// черновик агента подписан его именем — его видно на карточке
	id, _ := mine.Get("draft_id")
	if d, _ := box.Get(id.(string)); d.AgentName() != "box-agent" {
		t.Fatalf("origin черновика: %q", d.Origin)
	}
}

func must[T any](v T, _ bool) T { return v }
