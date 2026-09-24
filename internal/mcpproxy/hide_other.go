//go:build !windows

package mcpproxy

import "os/exec"

func hideWindow(*exec.Cmd) {}
