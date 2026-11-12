//go:build !windows

package procs

import "errors"

// Вне Windows tg-agent работает только в контейнерах, где старых сборок
// рядом нет.
func list(string) ([]Proc, error) { return nil, nil }

func alive(Proc) bool { return false }

func terminate(Proc) error { return errors.New("не поддерживается") }
