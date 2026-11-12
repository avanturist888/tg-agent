package procs

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestVerbProfile(t *testing.T) {
	cases := []struct {
		args          []string
		verb, profile string
	}{
		{[]string{`C:\x\bin\tg.exe`, "mcp"}, "mcp", ""},
		{[]string{"tgw.exe", "--profile", "work", "serve", "--worker"}, "serve", "work"},
		{[]string{"tg.exe", "--profile=max", "tool-call", "tg_read_chat"}, "tool-call", "max"},
		{[]string{"tg.exe"}, "", ""},
		{nil, "", ""},
	}
	for _, c := range cases {
		p := Proc{Args: c.args}
		if p.Verb() != c.verb || p.Profile() != c.profile {
			t.Errorf("%v: verb %q profile %q", c.args, p.Verb(), p.Profile())
		}
	}
	for name, want := range map[string]bool{
		"tg.exe": true, "TGW.EXE": true, "tg.old-20261112-150405.exe": true, "tgw.new.exe": true,
		"tgstt.exe": false, "tgx.exe": false, "tg.exe.bak": false,
	} {
		if IsProgram(name) != want {
			t.Errorf("IsProgram(%q) != %v", name, want)
		}
	}
}

// Помощник для TestListFindsInstall: процесс, который просто ждёт.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("PROCS_HELPER") != "1" {
		t.Skip("запускается только из TestListFindsInstall")
	}
	time.Sleep(30 * time.Second)
}

// Процесс программы из bin установки находится с командной строкой, а
// Terminate его завершает.
func TestListFindsInstall(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("только Windows")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "bin")
	os.MkdirAll(bin, 0o700)
	exe := filepath.Join(bin, "tg.exe")
	copyFile(t, self, exe)
	cmd := exec.Command(exe, "-test.run=^TestHelperProcess$", "--", "--profile", "work", "tool-call", "tg_read_chat")
	cmd.Env = append(os.Environ(), "PROCS_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	var found *Proc
	for range 50 {
		list, err := List(bin)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range list {
			if p.PID == cmd.Process.Pid {
				found = &p
			}
		}
		if found != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if found == nil {
		t.Fatal("процесс установки не найден")
	}
	if found.Verb() != "tool-call" || found.Profile() != "work" || found.Started.IsZero() || !Alive(*found) {
		t.Fatalf("процесс: %+v (verb %q)", found, found.Verb())
	}
	if list, _ := List(filepath.Join(t.TempDir(), "bin")); len(list) != 0 {
		t.Fatalf("процессы чужого каталога: %+v", list)
	}
	if err := Terminate(*found); err != nil {
		t.Fatal(err)
	}
	if Alive(*found) {
		t.Fatal("процесс жив после Terminate")
	}
	// тот же pid, но другое время запуска — не тот процесс
	stale := *found
	stale.Started = stale.Started.Add(-time.Hour)
	if Alive(stale) {
		t.Fatal("переиспользованный pid принят за прежний процесс")
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
