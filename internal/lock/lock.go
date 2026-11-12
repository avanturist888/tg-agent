// Package lock — межпроцессные локи на файлах.
//
// Лок — это файл <path>.lock, созданный с O_EXCL; внутри {"pid": …, "ts": …}.
// Процессов много (служба, MCP-прослойка на каждую сессию Claude Code, CLI,
// окно), а файлы в data/ — общие.
package lock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// StaleAfter — возраст лока без записанного владельца, после которого он протух.
const StaleAfter = 120 * time.Second

// Busy — лок держит кто-то другой дольше, чем мы согласны ждать.
type Busy struct{ Msg string }

func (e *Busy) Error() string { return e.Msg }

func IsBusy(err error) bool {
	var b *Busy
	return errors.As(err, &b)
}

var (
	// heldMu — и над held, и над шагом «создать файл → записать → отметить в
	// held»: иначе соседняя горутина увидела бы файл с нашим pid ещё без
	// отметки, сочла бы его осиротевшим и сняла живой лок.
	heldMu sync.Mutex
	// локи, которые этот процесс держит прямо сейчас (путь → кто держит):
	// отличают свой живой лок от своего же осиротевшего — pid в файле в обоих
	// случаях наш
	held = map[string]*File{}
)

// File — лок на файл path (сам лок — path + ".lock").
type File struct {
	Path    string
	Timeout time.Duration
	taken   bool
}

func New(path string, timeout time.Duration) *File {
	return &File{Path: path + ".lock", Timeout: timeout}
}

type meta struct {
	PID int     `json:"pid"`
	TS  float64 `json:"ts"`
}

func (l *File) tryAcquire() (bool, error) {
	heldMu.Lock()
	f, err := os.OpenFile(l.Path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		heldMu.Unlock()
		if errors.Is(err, os.ErrExist) || isSharingViolation(err) || errors.Is(err, os.ErrPermission) {
			l.breakIfStale()
			return false, nil
		}
		return false, err
	}
	defer heldMu.Unlock()
	now := float64(time.Now().UnixNano()) / 1e9
	body, _ := json.Marshal(meta{PID: os.Getpid(), TS: now})
	_, werr := f.Write(body)
	// файл сразу закрываем: сам факт его существования и есть лок, а открытый
	// дескриптор на Windows только мешает соседям читать и удалять
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(l.Path)
		return false, errors.Join(werr, cerr)
	}
	held[l.Path] = l
	l.taken = true
	return true, nil
}

func (l *File) breakIfStale() {
	st, err := os.Stat(l.Path)
	if err != nil {
		return
	}
	age := time.Since(st.ModTime())
	data, err := os.ReadFile(l.Path)
	if err != nil {
		return
	}
	var m meta
	_ = json.Unmarshal(data, &m)
	var stale bool
	switch {
	case m.PID == os.Getpid():
		// наш pid, но мы его не держим — снять файл при выходе не удалось.
		// Проверка и снятие — под heldMu: между ними своя же горутина не
		// возьмёт лок заново
		heldMu.Lock()
		defer heldMu.Unlock()
		if held[l.Path] == nil {
			_ = os.Remove(l.Path)
		}
		return
	case m.PID != 0:
		// Протухшим считаем только лок мёртвого владельца: живой процесс может
		// законно держать сессию долго, сносить его лок по возрасту нельзя.
		stale = !pidAlive(m.PID, m.TS)
	default:
		// владельца не записали (упал между созданием файла и записью pid)
		stale = age > StaleAfter
	}
	if stale {
		_ = os.Remove(l.Path) // не вышло — файл ещё открыт, значит, не протух
	}
}

// Acquire ждёт лок до Timeout или отмены ctx.
func (l *File) Acquire(ctx context.Context) error {
	deadline := time.Now().Add(l.Timeout)
	for {
		ok, err := l.tryAcquire()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return &Busy{fmt.Sprintf("Сессия Telegram занята другим процессом tg-agent "+
				"(лок %s). Подожди или удали лок-файл вручную.", l.Path)}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Touch продлевает лок: демон держит его часами.
func (l *File) Touch() {
	if l.taken {
		now := time.Now()
		_ = os.Chtimes(l.Path, now, now)
	}
}

// Release снимает лок. Падать нельзя: работа под локом уже сделана
// (например, сообщение уже отправлено).
func (l *File) Release() {
	if !l.taken {
		return
	}
	l.taken = false
	// сначала файл, потом отметка: пока файл наш, своя же горутина не сочтёт
	// его осиротевшим; отметку снимаем, только если она всё ещё наша (после
	// удаления файла лок могла взять соседняя горутина)
	forget := func() {
		heldMu.Lock()
		if held[l.Path] == l {
			delete(held, l.Path)
		}
		heldMu.Unlock()
	}
	defer forget()
	// Windows не даёт удалить файл, пока его читает другой процесс, — это
	// доли секунды, переждём
	for i := 0; i < 40; i++ {
		err := os.Remove(l.Path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	// не вышло — файл останется с нашим pid; held его забудет, так что и
	// мы, и другие (после нашей смерти) снимут его как протухший
}

// With — выполнить fn под локом.
func With(ctx context.Context, path string, timeout time.Duration, fn func() error) error {
	l := New(path, timeout)
	if err := l.Acquire(ctx); err != nil {
		return err
	}
	defer l.Release()
	return fn()
}

// Held — держит ли кто-нибудь лок прямо сейчас (для doctor и GUI).
func Held(path string) bool {
	l := New(path, 0)
	ok, err := l.tryAcquire()
	if err != nil {
		return false
	}
	if ok {
		l.Release()
		return false
	}
	return true
}

// Peek — лок path, не трогая его: есть ли файл и жив ли его владелец.
// В отличие от Held не снимает мёртвый лок — он остаётся следом (например,
// прерванной отправки). Нечитаемый файл (его как раз пишут) — живой.
func Peek(path string) (present, alive bool) {
	raw, err := os.ReadFile(path + ".lock")
	if err != nil {
		return !errors.Is(err, os.ErrNotExist), !errors.Is(err, os.ErrNotExist)
	}
	var m meta
	if json.Unmarshal(raw, &m) != nil || m.PID == 0 {
		// владельца не записали: его как раз пишут — или процесс упал между
		// созданием файла и записью (тогда, как у breakIfStale, по возрасту)
		st, err := os.Stat(path + ".lock")
		return true, err != nil || time.Since(st.ModTime()) <= StaleAfter
	}
	return true, pidAlive(m.PID, m.TS)
}

// PIDAlive — жив ли процесс pid, взявший что-то в момент since (unix, с
// долями секунды): умерший — нет, и процесс, получивший тот же pid позже
// since, — тоже нет.
func PIDAlive(pid int, since float64) bool { return pidAlive(pid, since) }
