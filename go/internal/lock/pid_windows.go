//go:build windows

package lock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

const stillActive = 259

// pidAlive — жив ли владелец лока. since — когда он взял лок: Windows быстро
// раздаёт pid умерших процессов заново, и процесс, запущенный позже взятия
// лока, — не владелец.
func pidAlive(pid int, since float64) bool {
	if pid == os.Getpid() {
		return true
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// нет процесса — мёртв; нет прав — считаем живым (чужой процесс)
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil || code != stillActive {
		return false
	}
	if since == 0 {
		return true
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return true
	}
	started := float64(created.Nanoseconds()) / 1e9
	return started <= since+1
}

func isSharingViolation(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
