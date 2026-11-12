//go:build !windows

package lock

import (
	"errors"
	"os"
	"syscall"
)

// openExisting — открыть существующий файл лока (O_CLOEXEC Go ставит сам).
func openExisting(path string) (*os.File, error) { return os.OpenFile(path, os.O_RDWR, 0) }

// placeNew — положить готовый файл tmp по пути path, только если там
// ничего нет (иначе — os.ErrExist). tmp остаётся: его снимает вызывающий.
func placeNew(tmp, path string) error { return os.Link(tmp, path) }

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
