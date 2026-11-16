//go:build windows

package tgc

import (
	"os/exec"
	"syscall"
)

func hideAudioProbe(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
