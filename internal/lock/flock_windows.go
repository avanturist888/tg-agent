//go:build windows

package lock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Запирается один байт далеко за концом файла: блокировки Windows
// обязательные, и запертый байт с данными не дал бы другим прочесть запись
// о владельце.
const lockOffsetHigh = 0x40000000

// openLock — открыть файл лока так, чтобы другие могли его и читать, и
// удалять (держатель удаляет его, отпуская лок, пока ждущие держат свои
// дескрипторы). create — создать, если нет; created — создали его мы.
// Дескриптор не наследуется дочерними процессами.
func openLock(path string, create bool) (f *os.File, created bool, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, false, err
	}
	open := func(disp uint32) (windows.Handle, error) {
		return windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil, disp, windows.FILE_ATTRIBUTE_NORMAL, 0)
	}
	for i := 0; ; i++ {
		if create {
			h, err := open(windows.CREATE_NEW)
			if err == nil {
				return os.NewFile(uintptr(h), path), true, nil
			}
			if !errors.Is(err, windows.ERROR_FILE_EXISTS) {
				return nil, false, &os.PathError{Op: "open", Path: path, Err: err}
			}
		}
		h, err := open(windows.OPEN_EXISTING)
		if err == nil {
			return os.NewFile(uintptr(h), path), false, nil
		}
		// между двумя открытиями файл удалил отпустивший лок — создать заново
		if !create || i >= 9 || !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil, false, &os.PathError{Op: "open", Path: path, Err: err}
		}
	}
}

// busyOpen — файл не открыть, потому что его как раз удаляют (держатель
// отпустил лок, а чужие дескрипторы ещё открыты) или держат без общего
// доступа.
func busyOpen(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

func tryLock(f *os.File) (bool, error) {
	ol := windows.Overlapped{OffsetHigh: lockOffsetHigh}
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, &os.PathError{Op: "lock", Path: f.Name(), Err: err}
}

func unlock(f *os.File) {
	ol := windows.Overlapped{OffsetHigh: lockOffsetHigh}
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}
