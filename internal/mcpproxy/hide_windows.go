//go:build windows

package mcpproxy

import (
	"os/exec"
	"syscall"
)

// hideWindow — дочерний tg.exe без собственного окна консоли.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
