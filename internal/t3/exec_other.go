//go:build !windows

package t3

import "os/exec"

func hideWindow(*exec.Cmd) {}
