//go:build !windows

package stt

import "os/exec"

func hideWindow(*exec.Cmd) {}
