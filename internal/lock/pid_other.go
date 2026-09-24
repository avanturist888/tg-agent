//go:build !windows

package lock

import (
	"errors"
	"os"
	"syscall"
)

func pidAlive(pid int, since float64) bool {
	if pid == os.Getpid() {
		return true
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func isSharingViolation(error) bool { return false }
