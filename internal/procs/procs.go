// Package procs — запущенные процессы этой установки tg-agent: bin\tg.exe,
// bin\tgw.exe и их прежние сборки (scripts\build.ps1 переименовывает
// занятый exe в *.old-<время>.exe, процесс продолжает работать).
//
// Нужен обновлению (tg cutover): перед переносом состояния старой сборки
// её процессы должны закончить работу — иначе они пишут файлы, которые
// новая уже перенесла, или отправляют черновик, который она тоже отправит.
package procs

import (
	"regexp"
	"strings"
	"time"
)

// Proc — процесс установки.
type Proc struct {
	PID     int
	Exe     string    // полный путь образа
	Args    []string  // командная строка; nil — прочитать не удалось
	Started time.Time // время запуска (по нему же отличаем переиспользованный pid)
}

// Verb — команда tg (serve, mcp, gui, tool-call…), флаг --profile
// пропускается; "" — командная строка не прочиталась или команды нет.
func (p Proc) Verb() string {
	if len(p.Args) < 2 {
		return ""
	}
	args := p.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--profile" || a == "-profile" {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a
	}
	return ""
}

// Profile — профиль из флага --profile ("" — флага нет: основной или из
// TG_PROFILE, окружение чужого процесса не читаем).
func (p Proc) Profile() string {
	for i, a := range p.Args {
		if v, ok := strings.CutPrefix(a, "--profile="); ok {
			return v
		}
		if (a == "--profile" || a == "-profile") && i+1 < len(p.Args) {
			return p.Args[i+1]
		}
	}
	return ""
}

var programRe = regexp.MustCompile(`(?i)^tgw?(\.[^\\/]*)?\.exe$`)

// IsProgram — имя файла — программа tg-agent: tg.exe, tgw.exe,
// tg.old-20261112-150405.exe… (tgstt.exe — нет: это расшифровка, она
// состояния не пишет).
func IsProgram(name string) bool { return programRe.MatchString(name) }

// List — процессы программ из каталога bin, кроме этого.
func List(bin string) ([]Proc, error) { return list(bin) }

// Alive — процесс ещё работает (тот же, а не новый с тем же pid).
func Alive(p Proc) bool { return alive(p) }

// Terminate — завершить процесс (если это всё ещё он).
func Terminate(p Proc) error { return terminate(p) }
