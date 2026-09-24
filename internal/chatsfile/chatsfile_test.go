package chatsfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tgagent/internal/config"
)

const sample = `# Белый список
[[chat]]
alias = "first"
id = -100123
title = "Первый" # комментарий
read = true
send = false

# второй
[[chat]]
alias = 'second'
id = "me"
title = "Избранное"
read = true
send = true
note = "песочница"
`

func setup(t *testing.T) string {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "config"), 0o700)
	path := filepath.Join(root, "config", "chats.toml")
	os.WriteFile(path, []byte(sample), 0o600)
	old := config.Root
	config.Root = root
	t.Cleanup(func() { config.Root = old })
	return path
}

func TestUpdateAddRemove(t *testing.T) {
	path := setup(t)
	if err := Update("first", true, true, true, nil, nil); err != nil {
		t.Fatal(err)
	}
	rules, _, err := config.LoadChats(path)
	if err != nil || !rules["first"].Send || !rules["first"].Auto || rules["second"].Auto {
		t.Fatalf("%v %+v", err, rules)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# комментарий") || !strings.Contains(string(raw), "# второй") {
		t.Fatalf("комментарии потеряны:\n%s", raw)
	}
	note := "новая"
	if err := Update("second", true, false, true, nil, &note); err != nil {
		t.Fatal(err)
	}
	rules, _, _ = config.LoadChats(path)
	if rules["second"].Send || rules["second"].AutoSend() || rules["second"].Note != "новая" {
		t.Fatalf("%+v", rules["second"])
	}
	if err := Add(Chat{Alias: "third", Peer: int64(42), Title: `с "кавычками"`, Read: true}); err != nil {
		t.Fatal(err)
	}
	if err := Add(Chat{Alias: "THIRD", Peer: int64(1)}); err == nil {
		t.Fatal("повтор алиаса прошёл")
	}
	if err := Remove("first"); err != nil {
		t.Fatal(err)
	}
	rules, order, err := config.LoadChats(path)
	if err != nil || len(rules) != 2 || order[0] != "second" || rules["third"].Peer != int64(42) {
		t.Fatalf("%v %v", err, order)
	}
}
