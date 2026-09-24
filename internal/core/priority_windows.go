//go:build windows

package core

import "golang.org/x/sys/windows"

// RaisePriority — Планировщик запускает задачи с пониженным приоритетом, и
// Windows морозила процесс на десятки секунд при почти пустом процессоре.
func RaisePriority() {
	_ = windows.SetPriorityClass(windows.CurrentProcess(), windows.NORMAL_PRIORITY_CLASS)
}
