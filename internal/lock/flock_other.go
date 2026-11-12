//go:build !windows

package lock

import (
	"errors"
	"os"
	"syscall"
)

func openLock(path string, create bool) (*os.File, error) {
	flag := os.O_RDWR
	if create {
		flag |= os.O_CREATE
	}
	return os.OpenFile(path, flag, 0o600) // O_CLOEXEC — Go ставит сам
}

func busyOpen(error) bool { return false }

func tryLock(f *os.File) (bool, error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return false, nil
		default:
			return false, &os.PathError{Op: "flock", Path: f.Name(), Err: err}
		}
	}
}

func unlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
