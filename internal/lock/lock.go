// Package lock — межпроцессные локи.
//
// Лок — блокировка ОС на файле <path>.lock (Windows — LockFileEx, иначе —
// flock), а не сам факт существования файла. Её держит открытый дескриптор,
// и снимает её ОС, когда процесс умирает. Поэтому протухших локов не бывает
// и снимать чужое по pid или по возрасту не нужно. Это и закрывает гонку
// прежней схемы (файл с O_EXCL), где «снять протухший» могло удалить уже
// новый, живой лок другого процесса.
//
// Процессов много (служба, MCP-прослойка на каждую сессию Claude Code, CLI,
// окно), а файлы в data/ — общие. Два дескриптора одного процесса тоже
// исключают друг друга, так что лок разделяет и горутины.
//
// Файл лока — только место для блокировки. Отпуская лок, держатель его
// удаляет (иначе файлы карточек копились бы), а захват проверяет, что путь
// всё ещё ведёт к тому файлу, который он запер: иначе запер удалённый, и
// захват начинается заново. Не удалился файл — не беда, его запрут заново.
// Пока лок взят, в файле — {"pid": …, "ts": …, "os": true}: для диагностики
// и для сборок до OS-локов, которые узнают живой лок по pid. Отпущенный —
// {"os": true}.
//
// Совместимость со сборками до OS-локов (файл с O_EXCL, внутри pid, файл
// не держится открытым). Пока такая сборка жива (обновление: exe уже
// заменены, а старые процессы ещё доживают — tg cutover), её лок — только
// запись в файле. Поэтому, заперев файл, захват читает его: запись без
// "os" с живым pid другого процесса — лок старой сборки, он занят. Пустой
// файл, созданный не нами, — занят, пока молод (freshEmpty): старая сборка
// пишет pid сразу после создания, а сборка с OS-локами — сразу после
// блокировки. Давний пустой — брошенный, свободен.
package lock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// StaleAfter — возраст файла лока старой сборки (O_EXCL) без записанного
// владельца, после которого он считается брошенным (Peek).
const StaleAfter = 120 * time.Second

// Busy — лок держит кто-то другой дольше, чем мы согласны ждать.
type Busy struct{ Msg string }

func (e *Busy) Error() string { return e.Msg }

func IsBusy(err error) bool {
	var b *Busy
	return errors.As(err, &b)
}

// File — лок на файл path (сам лок — path + ".lock").
type File struct {
	Path    string
	Timeout time.Duration
	f       *os.File // открыт и заперт, пока лок наш
}

func New(path string, timeout time.Duration) *File {
	return &File{Path: path + ".lock", Timeout: timeout}
}

type meta struct {
	PID int     `json:"pid,omitempty"`
	TS  float64 `json:"ts,omitempty"`
	OS  bool    `json:"os,omitempty"` // записан сборкой с OS-локами
}

// released — запись отпущенного лока.
var released = []byte(`{"os":true}`)

// freshEmpty — сколько пустой файл, созданный не нами, считается занятым
// (его создатель вот-вот запишет владельца).
const freshEmpty = 5 * time.Second

// oldOwner — файл, возможно, держит процесс сборки до OS-локов (или его
// только что создал другой процесс и ещё не записал владельца).
func oldOwner(f *os.File, created bool) bool {
	raw, err := readAll(f)
	if err != nil {
		return true // не прочесть — не судить: следующая попытка
	}
	if len(raw) == 0 {
		if created {
			return false
		}
		st, err := f.Stat()
		return err != nil || time.Since(st.ModTime()) < freshEmpty
	}
	var m meta
	if json.Unmarshal(raw, &m) != nil || m.OS || m.PID == 0 || m.PID == os.Getpid() {
		return false
	}
	return pidAlive(m.PID, m.TS)
}

// tryAcquire — одна попытка без ожидания.
func (l *File) tryAcquire() (bool, error) {
	if l.f != nil {
		return false, fmt.Errorf("лок %s уже взят этим же объектом", l.Path)
	}
	f, created, err := openLock(l.Path, true)
	if err != nil {
		if errors.Is(err, os.ErrExist) || busyOpen(err) {
			return false, nil // файл как раз удаляют, отпуская лок
		}
		if _, derr := os.Stat(filepath.Dir(l.Path)); errors.Is(err, os.ErrNotExist) && derr == nil {
			return false, nil // его раз за разом удаляли между открытиями
		}
		return false, err
	}
	ok, err := tryLock(f)
	if err != nil || !ok {
		_ = f.Close()
		return false, err
	}
	// прежний держатель мог удалить файл, пока мы его открывали: тогда
	// заперт удалённый, а по пути уже лежит (или вот-вот ляжет) новый
	if !samePath(f, l.Path) || oldOwner(f, created) {
		unlock(f)
		_ = f.Close()
		return false, nil
	}
	body, _ := json.Marshal(meta{PID: os.Getpid(), TS: float64(time.Now().UnixNano()) / 1e9, OS: true})
	if f.Truncate(0) == nil {
		_, _ = f.WriteAt(body, 0) // только сведения: лок держит блокировка
	}
	l.f = f
	return true, nil
}

func samePath(f *os.File, path string) bool {
	a, err := f.Stat()
	if err != nil {
		return false
	}
	b, err := os.Stat(path)
	return err == nil && os.SameFile(a, b)
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
				"(лок %s). Подожди и повтори.", l.Path)}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Release снимает лок. Падать нельзя: работа под локом уже сделана
// (например, сообщение уже отправлено).
func (l *File) Release() {
	f := l.f
	if f == nil {
		return
	}
	l.f = nil
	// файл удаляем, пока он заперт нами: тот, кто запрёт его после нас,
	// увидит, что путь ведёт уже не к нему, и начнёт заново. Не удалился
	// (его читают) — останется отпущенным, без нашего pid, и пригодится
	// следующему
	if f.Truncate(0) == nil {
		_, _ = f.WriteAt(released, 0)
	}
	_ = os.Remove(l.Path)
	unlock(f)
	_ = f.Close()
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

// Peek — лок path, не трогая файл: есть ли он и жив ли его владелец.
// В отличие от Held не удаляет файл — он остаётся следом (например,
// прерванной отправки старой сборки). Заперт — владелец жив. Не заперт и
// записан сборкой с OS-локами — свободен. Иначе судим по записи сборки до
// OS-локов: pid жив — жив; владельца не записали — по возрасту (его как раз
// пишут или процесс упал между созданием и записью).
func Peek(path string) (present, alive bool) {
	p := path + ".lock"
	f, _, err := openLock(p, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, false
		}
		return true, true // не открыть — его как раз удаляют или пишут
	}
	defer f.Close()
	ok, err := tryLock(f)
	if err != nil || !ok {
		return true, true
	}
	defer unlock(f)
	raw, err := readAll(f)
	if err != nil {
		return true, true
	}
	var m meta
	if json.Unmarshal(raw, &m) == nil && m.OS {
		return true, false
	}
	if m.PID == 0 {
		st, err := f.Stat()
		return true, err != nil || time.Since(st.ModTime()) <= StaleAfter
	}
	return true, pidAlive(m.PID, m.TS)
}

func readAll(f *os.File) ([]byte, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	buf := make([]byte, st.Size())
	n, err := f.ReadAt(buf, 0)
	if n == len(buf) {
		return buf, nil
	}
	return buf[:n], err
}

// PIDAlive — жив ли процесс pid, взявший что-то в момент since (unix, с
// долями секунды): умерший — нет, и процесс, получивший тот же pid позже
// since, — тоже нет.
func PIDAlive(pid int, since float64) bool { return pidAlive(pid, since) }
