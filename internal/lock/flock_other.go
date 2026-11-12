//go:build !windows

package lock

import (
	"errors"
	"os"
	"syscall"
)

func openLock(path string, create bool) (*os.File, bool, error) {
	// O_CLOEXEC Go ставит сам
	for i := 0; ; i++ {
		if create {
			f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
			if err == nil {
				return f, true, nil
			}
			if !errors.Is(err, os.ErrExist) {
				return nil, false, err
			}
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		// между двумя открытиями файл удалил отпустивший лок — создать заново
		if err == nil || !create || i >= 9 || !errors.Is(err, os.ErrNotExist) {
			return f, false, err
		}
	}
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
