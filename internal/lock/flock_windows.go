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

// openExisting — открыть существующий файл лока так, чтобы другие могли его
// и читать, и удалять (держатель удаляет его, отпуская лок, пока ждущие
// держат свои дескрипторы). Дескриптор не наследуется дочерними процессами.
func openExisting(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// placeNew — положить готовый файл tmp по пути path, только если там
// ничего нет (иначе — os.ErrExist): перенос без замены (os.Rename заменил
// бы существующий).
func placeNew(tmp, path string) error {
	from, err := windows.UTF16PtrFromString(tmp)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, 0); err != nil {
		return &os.LinkError{Op: "move", Old: tmp, New: path, Err: err}
	}
	return nil
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
