//go:build !windows

package accounts

import "os/exec"

func hideWindow(*exec.Cmd) {}
