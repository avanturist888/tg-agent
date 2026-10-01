//go:build windows

package service

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// killJob — job object Windows с KILL_ON_JOB_CLOSE: когда надзиратель
// завершается (даже убитый), система закрывает job и добивает рабочий
// процесс — иначе тот остался бы сиротой, держащим канал службы.
type killJob struct{ h windows.Handle }

func newKillJob() (killJob, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return killJob{}, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(h)
		return killJob{}, err
	}
	// хэндл не закрываем: он живёт, пока жив надзиратель
	return killJob{h}, nil
}

func (j killJob) add(p *os.Process) error {
	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(ph)
	return windows.AssignProcessToJobObject(j.h, ph)
}

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
