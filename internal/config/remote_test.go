package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExamplesParse — образцы config/*.example.toml разбираются как настоящие.
func TestExamplesParse(t *testing.T) {
	repo, _ := filepath.Abs(filepath.Join("..", ".."))
	root := tempInstall(t)
	for _, name := range []string{"t3", "agents"} {
		raw, err := os.ReadFile(filepath.Join(repo, "config", name+".example.toml"))
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(root, "config", name+".toml"), string(raw))
	}
	envs, err := LoadT3()
	if err != nil || len(envs) != 1 || envs[0].Name != "bala" || len(envs[0].PairingCmd) == 0 {
		t.Fatalf("t3.example.toml: %+v %v", envs, err)
	}
	if envs[0].TokenPath() != filepath.Join(root, "data", "t3-token-bala.json") {
		t.Fatalf("токен окружения: %s", envs[0].TokenPath())
	}
	agents, err := LoadAgents()
	if err != nil || agents.Listen != "127.0.0.1:8790" || len(agents.List) != 1 || agents.List[0].Env != "bala" {
		t.Fatalf("agents.example.toml: %+v %v", agents, err)
	}
}

func TestRemoteConfigErrors(t *testing.T) {
	root := tempInstall(t)
	path := filepath.Join(root, "config", "t3.toml")
	for body, want := range map[string]string{
		"[[env]]\nname = \"local\"\norigin = \"http://x:1\"\ntoken_file = \"t\"\n":   "занято",
		"[[env]]\nname = \"a b\"\norigin = \"http://x:1\"\ntoken_file = \"t\"\n":     "латиница",
		"[[env]]\nname = \"a\"\norigin = \"x:1\"\ntoken_file = \"t\"\n":              "origin",
		"[[env]]\nname = \"a\"\norigin = \"http://x:1\"\n":                           "способа получить токен",
		"[[env]]\nname = \"a\"\norigin = \"http://x:1\"\ntoken_file = \"t\"\n[[env]]\nname = \"A\"\norigin = \"http://y:1\"\ntoken_file = \"t\"\n": "повторяется",
	} {
		write(t, path, body)
		if _, err := LoadT3(); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: ждали ошибку про %q, получили %v", body, want, err)
		}
	}
	apath := filepath.Join(root, "config", "agents.toml")
	write(t, apath, "[[agent]]\nname = \"a\"\nchats = [\"x\"]\n")
	if _, err := LoadAgents(); err == nil || !strings.Contains(err.Error(), "token_env") {
		t.Fatalf("агент без token_env: %v", err)
	}
	write(t, apath, "[http]\nlisten = \"8790\"\n")
	if _, err := LoadAgents(); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("listen без хоста: %v", err)
	}
}

func TestRestrict(t *testing.T) {
	s := &Settings{Chats: map[string]ChatRule{"A": {Alias: "A"}, "b": {Alias: "b"}, "c": {Alias: "c"}}, ChatOrder: []string{"A", "b", "c"}}
	r := s.Restrict(&Agent{Name: "x", Chats: []string{"a", "c", "нет"}})
	if strings.Join(r.ChatOrder, ",") != "A,c" || len(r.Chats) != 2 || r.AgentName() != "x" {
		t.Fatalf("урезанный список: %v", r.ChatOrder)
	}
	if len(s.Chats) != 3 || s.Agent != nil {
		t.Fatal("Restrict не должен трогать исходные настройки")
	}
	if _, err := r.Resolve("b"); err == nil {
		t.Fatal("чат вне списка агента")
	}
}
