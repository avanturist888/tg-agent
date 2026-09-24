//go:build !windows

package core

import (
	"os"
	"os/exec"
)

func spawnSendDue() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "send-due")
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
