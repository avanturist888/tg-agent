//go:build !live

package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"tgagent/internal/config"
)

// A test that omits watchEnv must still never open the installed profile.
// In particular, opening state.db can move all of its legacy JSON files.
// Live tests use the installation deliberately and have a separate build tag.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tg-agent-core-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	config.Root = dir
	for key, value := range map[string]string{
		"TG_AGENT_HOME":   dir,
		"T3CODE_HOME":     dir,
		"TG_BOT_ENV_FILE": filepath.Join(dir, "missing-bot.env"),
	} {
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.RemoveAll(dir)
			os.Exit(1)
		}
	}
	if err := config.SetProfile(""); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

func TestUnitTestsDoNotOpenInheritedInstallation(t *testing.T) {
	if os.Getenv("TG_CORE_ISOLATION_CHILD") == "1" {
		// Deliberately omit watchEnv: TestMain alone must isolate this access.
		if _, err := stateDB(&config.Settings{}); err != nil {
			t.Fatal(err)
		}
		if config.Profile() != "" {
			t.Fatal("inherited profile was not cleared")
		}
		return
	}
	installation := t.TempDir()
	want := []byte(`{"sentinel":{"chats":{"synthetic":90}}}`)
	var sentinels []string
	for _, home := range []string{installation, filepath.Join(installation, "profiles", "work")} {
		dir := filepath.Join(home, "data")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "subscriptions.json")
		if err := os.WriteFile(path, want, 0o600); err != nil {
			t.Fatal(err)
		}
		sentinels = append(sentinels, path)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestUnitTestsDoNotOpenInheritedInstallation$")
	cmd.Dir = installation
	cmd.Env = append(os.Environ(), "TG_CORE_ISOLATION_CHILD=1", "TG_AGENT_HOME="+installation,
		"TG_PROFILE=work", "T3CODE_HOME="+installation)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("test subprocess: %v\n%s", err, output)
	}
	for _, path := range sentinels {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(want) {
			t.Fatalf("inherited installation changed: %s: %v", path, err)
		}
		for _, unexpected := range []string{path + ".migrated", filepath.Join(filepath.Dir(path), "state.db")} {
			if _, err := os.Stat(unexpected); !os.IsNotExist(err) {
				t.Fatalf("test opened inherited installation: %s (%v)", unexpected, err)
			}
		}
	}
}
