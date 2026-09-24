//go:build !windows

package mcpserver

import "os/exec"

func hideWindow(*exec.Cmd) {}
