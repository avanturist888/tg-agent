//go:build !windows

package tgc

import "os/exec"

func hideAudioProbe(cmd *exec.Cmd) {}
