package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gotd/td/tg"

	"tgagent/internal/config"
	"tgagent/internal/outbox"
	"tgagent/internal/tgc"
	"tgagent/internal/wincred"
)

const (
	liveTok = "8111111111:AAHliveliveliveliveliveliveliveliveli"
	deadTok = "8222222222:AAHdeaddeaddeaddeaddeaddeaddeaddeadd"
	newTok  = "8333333333:AAHnewnewnewnewnewnewnewnewnewnewnew"
)

type fakeBotFather struct {
	answers map[string]string // шаг → ответ BotFather
	sent    []string
	deleted []int
	history []tg.MessageClass
}

func (f *fakeBotFather) BotDialog(_ context.Context, _ tgc.Target, steps []string, stop func(string) bool) (int, []tgc.BotReply, error) {
	var out []tgc.BotReply
	for i, st := range steps {
		f.sent = append(f.sent, st)
		r := tgc.BotReply{Step: st, ID: 100 + i, Text: f.answers[st]}
		out = append(out, r)
		if stop(r.Text) {
			break
		}
	}
	return 1, out, nil
}

func (f *fakeBotFather) DeleteMessages(_ context.Context, _ tgc.Target, ids []int) error {
	f.deleted = append(f.deleted, ids...)
	return nil
}

func (f *fakeBotFather) History(context.Context, tgc.Target, int, int, int, string, int) (*tgc.Batch, error) {
	return &tgc.Batch{Messages: f.history}, nil
}

func botTestEnv(t *testing.T) *config.Settings {
	t.Helper()
	old := config.Root
	config.Root = t.TempDir()
	t.Cleanup(func() { config.Root = old })
	os.MkdirAll(config.DataPath(), 0o700)
	oldMe := botGetMe
	botGetMe = func(_ context.Context, _ *config.Settings, tok string) (botMe, error) {
		switch tok {
		case liveTok:
			return botMe{ID: 8111111111, Username: "live_bot", FirstName: "Live"}, nil
		case newTok:
			return botMe{ID: 8333333333, Username: "fresh_bot", FirstName: "Fresh"}, nil
		}
		return botMe{}, errors.New("getMe: Unauthorized")
	}
	t.Cleanup(func() { botGetMe = oldMe })
	creds := map[string]string{} // настоящий диспетчер учётных данных тесты не трогают
	oldSet, oldGet := credSet, credGet
	credSet = func(target, secret string) error { creds[target] = secret; return nil }
	credGet = func(target string) (string, error) {
		if v, ok := creds[target]; ok {
			return v, nil
		}
		return "", wincred.ErrNotFound
	}
	t.Cleanup(func() { credSet, credGet = oldSet, oldGet })
	return &config.Settings{}
}

func TestVaultKeepsTokensOutOfFile(t *testing.T) {
	botTestEnv(t)
	// старая запись с токеном в файле переезжает в диспетчер
	os.WriteFile(vaultPath(), []byte(`{"old_bot":{"username":"@old_bot","bot_id":"1","token":"`+liveTok+`"}}`), 0o600)
	if tok, ok := VaultToken("@old_bot"); !ok || tok != liveTok {
		t.Fatalf("после переезда: %q %v", tok, ok)
	}
	if raw, _ := os.ReadFile(vaultPath()); strings.Contains(string(raw), "AAH") {
		t.Fatalf("токен остался в файле: %s", raw)
	}
}

func TestRunBotOpRevoke(t *testing.T) {
	s := botTestEnv(t)
	env := filepath.Join(t.TempDir(), "worker.env")
	os.WriteFile(env, []byte("A=1\nTG_BOT_TOKEN=old\n"), 0o600)
	f := &fakeBotFather{answers: map[string]string{
		"/revoke":    "Choose a bot to generate a new token.",
		"@fresh_bot": "Your token was replaced with a new one. You can use this token to access HTTP API:\n" + newTok,
	}}
	d := &outbox.Draft{ID: "d1", Bot: &outbox.BotSpec{Action: "revoke", Bot: "@fresh_bot", EnvFile: env, EnvKey: "TG_BOT_TOKEN"}}
	out, err := runBotOp(context.Background(), s, f, tgc.Target{}, d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.sent, "|") != "/revoke|@fresh_bot" || len(f.deleted) != 1 || f.deleted[0] != 101 {
		t.Fatalf("диалог %v, удалено %v", f.sent, f.deleted)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "AAHnew") {
		t.Fatalf("токен в ответе агенту: %s", raw)
	}
	if saved, _ := out.Get("token_saved"); saved != "@fresh_bot" {
		t.Fatalf("token_saved = %v", saved)
	}
	if tok, ok := VaultToken("@fresh_bot"); !ok || tok != newTok {
		t.Fatal("нет в хранилище")
	}
	if raw, _ := os.ReadFile(vaultPath()); strings.Contains(string(raw), "AAHnew") {
		t.Fatalf("токен в файле хранилища: %s", raw)
	}
	if b, _ := os.ReadFile(env); string(b) != "A=1\nTG_BOT_TOKEN="+newTok+"\n" {
		t.Fatalf(".env: %q", b)
	}
	if r := botOpResult(s, "d1"); r == nil || strings.Contains(string(r), "AAHnew") {
		t.Fatalf("итог для tg_wait_approval: %s", r)
	}
	list, _ := ListBots(s)
	if raw, _ := json.Marshal(list); strings.Contains(string(raw), "AAHnew") || !strings.Contains(string(raw), "fresh_bot") {
		t.Fatalf("tg_bots: %s", raw)
	}
}

func TestRunBotOpRefused(t *testing.T) {
	s := botTestEnv(t)
	f := &fakeBotFather{answers: map[string]string{
		"/newbot":   "Alright, a new bot. How are we going to call it?",
		"Имя":       "Good. Now let's choose a username for your bot.",
		"taken_bot": "Sorry, this username is already taken. Please try something different.",
	}}
	d := &outbox.Draft{ID: "d2", Bot: &outbox.BotSpec{Action: "newbot", Name: "Имя", Username: "taken_bot"}}
	_, err := runBotOp(context.Background(), s, f, tgc.Target{}, d)
	var den *Denied
	if !errors.As(err, &den) || !strings.Contains(err.Error(), "already taken") {
		t.Fatalf("отказ: %v", err)
	}
	if f.sent[len(f.sent)-1] != "/cancel" {
		t.Fatalf("после отказа нет /cancel: %v", f.sent)
	}
}

func TestPurgeTokens(t *testing.T) {
	s := botTestEnv(t)
	f := &fakeBotFather{history: []tg.MessageClass{ // от новых к старым
		&tg.Message{ID: 30, Message: "Done! Use this token:\n" + liveTok},
		&tg.Message{ID: 20, Message: "/token", Out: true},
		&tg.Message{ID: 15, Message: "old one " + deadTok},
		&tg.Message{ID: 10, Message: "Hello"},
	}}
	out, err := purgeTokens(context.Background(), s, f, tgc.Target{}, "d3")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 2 || f.deleted[0] != 30 || f.deleted[1] != 15 {
		t.Fatalf("удалено %v", f.deleted)
	}
	if k, _ := out.Get("tokens_kept"); k != 1 {
		t.Fatalf("итог %v", out)
	}
	if tok, ok := VaultToken("@live_bot"); !ok || tok != liveTok {
		t.Fatal("действующий токен не сохранён перед удалением")
	}
}

func TestDraftBotOpChecks(t *testing.T) {
	s := &config.Settings{Chats: map[string]config.ChatRule{
		"botfather": {Alias: "botfather", Peer: int64(93372553), Read: true, Send: true},
	}, ChatOrder: []string{"botfather"}}
	ctx := context.Background()
	for name, b := range map[string]outbox.BotSpec{
		"нет действия":      {Action: "hack"},
		"нет бота":          {Action: "revoke"},
		"плохой username":   {Action: "newbot", Name: "X", Username: "nope"},
		"нет значения":      {Action: "setname", Bot: "@a_bot"},
		"env не .env":       {Action: "token", Bot: "@a_bot", EnvFile: `C:\Windows\system.ini`, EnvKey: "K"},
		"env относительный": {Action: "token", Bot: "@a_bot", EnvFile: ".env", EnvKey: "K"},
		"env не для токена": {Action: "setname", Bot: "@a_bot", Value: "x", EnvFile: `C:\a\.env`, EnvKey: "K"},
	} {
		if _, err := DraftBotOp(ctx, s, b, ""); err == nil {
			t.Errorf("%s: принято", name)
		}
	}
	remote := &config.Settings{Agent: &config.Agent{Name: "bala"}}
	if err := checkEnvTarget(remote, `C:\a\.env`, "K"); err == nil {
		t.Error("агент из контейнера пишет файл шлюза")
	}
	ro := &config.Settings{Chats: map[string]config.ChatRule{"bf": {Alias: "bf", Peer: "@BotFather", Read: true}}, ChatOrder: []string{"bf"}}
	if _, err := botFatherRule(ro); err == nil {
		t.Error("BotFather без отправки принят")
	}
}
