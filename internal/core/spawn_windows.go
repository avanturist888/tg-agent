//go:build windows

package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"tgagent/internal/config"
)

// spawnSendDue — отдельный процесс без окна, переживающий вызвавший его.
func spawnSendDue() error {
	exe := filepath.Join(config.Root, "bin", "tgw.exe")
	if _, err := os.Stat(exe); err != nil {
		exe, _ = os.Executable()
	}
	cmd := exec.Command(exe, "send-due")
	cmd.Dir = config.Root
	// DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP | CREATE_NO_WINDOW
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000008 | 0x00000200 | 0x08000000}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
