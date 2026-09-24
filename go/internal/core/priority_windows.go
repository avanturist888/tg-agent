//go:build windows

package core

import "golang.org/x/sys/windows"

// raisePriority — Планировщик запускает задачи с пониженным приоритетом, и
// Windows морозила слушателя на десятки секунд при почти пустом процессоре.
func raisePriority() {
	_ = windows.SetPriorityClass(windows.CurrentProcess(), windows.NORMAL_PRIORITY_CLASS)
}
