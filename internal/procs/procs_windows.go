package procs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const stillActive = 259

func list(bin string) ([]Proc, error) {
	bin = filepath.Clean(bin)
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	self := os.Getpid()
	var out []Proc
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		pid := int(e.ProcessID)
		if pid == self || !IsProgram(windows.UTF16ToString(e.ExeFile[:])) {
			continue
		}
		if p, ok := inspect(pid, bin); ok {
			out = append(out, p)
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return out, err
	}
	return out, nil
}

// inspect — процесс pid, если его образ лежит в bin (другая установка или
// чужая программа с похожим именем — нет).
func inspect(pid int, bin string) (Proc, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return Proc{}, false // уже завершился или чужой пользователь
	}
	defer windows.CloseHandle(h)
	exe, err := imagePath(h)
	if err != nil || !strings.EqualFold(filepath.Dir(exe), bin) {
		return Proc{}, false
	}
	p := Proc{PID: pid, Exe: exe, Started: started(h)}
	p.Args, _ = commandLine(h)
	return p, true
}

func imagePath(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return "", err
	}
	return filepath.Clean(windows.UTF16ToString(buf[:size])), nil
}

func started(h windows.Handle) time.Time {
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return time.Time{}
	}
	return time.Unix(0, created.Nanoseconds())
}

// commandLine — командная строка процесса (ProcessCommandLineInformation:
// Windows 8.1+, хватает PROCESS_QUERY_LIMITED_INFORMATION).
func commandLine(h windows.Handle) ([]string, error) {
	buf := make([]uint64, 512) // uint64 — выравнивание под UNICODE_STRING
	for {
		var need uint32
		err := windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation,
			unsafe.Pointer(&buf[0]), uint32(len(buf)*8), &need)
		if err == nil {
			break
		}
		if errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) && need > uint32(len(buf)*8) && need < 1<<20 {
			buf = make([]uint64, need/8+1)
			continue
		}
		return nil, err
	}
	us := (*windows.NTUnicodeString)(unsafe.Pointer(&buf[0]))
	if us.Buffer == nil || us.Length == 0 {
		return []string{}, nil
	}
	line := windows.UTF16ToString(unsafe.Slice(us.Buffer, us.Length/2))
	return windows.DecomposeCommandLine(line)
}

func open(p Proc, access uint32) (windows.Handle, bool) {
	h, err := windows.OpenProcess(access|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(p.PID))
	if err != nil {
		return 0, false
	}
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil || code != stillActive ||
		(!p.Started.IsZero() && !started(h).Equal(p.Started)) {
		windows.CloseHandle(h)
		return 0, false
	}
	return h, true
}

func alive(p Proc) bool {
	h, ok := open(p, 0)
	if ok {
		windows.CloseHandle(h)
	}
	return ok
}

func terminate(p Proc) error {
	h, ok := open(p, windows.PROCESS_TERMINATE|windows.SYNCHRONIZE)
	if !ok {
		return nil // уже нет
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		return err
	}
	_, _ = windows.WaitForSingleObject(h, 5000)
	return nil
}
