package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"tgagent/internal/config"
	"tgagent/internal/procs"
	"tgagent/internal/store"
)

// cmdRollback — tg rollback: перед возвратом на сборку до state.db
// выгрузить состояние из базы в её файлы (store.ExportLegacy). Процессы
// установки (кроме прослоек агентов и себя) должны быть остановлены: живой
// писал бы в базу уже после выгрузки.
func cmdRollback(ctx context.Context, args []string) int {
	list, err := procs.List(filepath.Join(config.Root, "bin"))
	if err != nil {
		return fail(fmt.Errorf("процессы установки не перечислить: %w", err))
	}
	if busy := rollbackBlockers(list, os.Getpid()); len(busy) > 0 {
		return fail(fmt.Errorf("сначала останови процессы установки (служба: Stop-ScheduledTask и Stop-Process, окно — закрыть):%s", strings.Join(busy, "")))
	}
	var failed []string
	for _, acct := range config.Accounts() {
		dir := filepath.Join(config.HomeOf(acct), "data")
		if _, err := os.Stat(filepath.Join(dir, "state.db")); err != nil {
			continue // этот профиль в базу не переезжал
		}
		restored, err := store.ExportLegacy(dir)
		if len(restored) > 0 {
			fmt.Printf("%s: %s\n", acct, strings.Join(restored, ", "))
		}
		if err != nil {
			// сбой одного профиля не мешает остальным; повтор доделает
			fmt.Fprintf(os.Stderr, "%s: %v\n", acct, err)
			failed = append(failed, acct)
		}
	}
	if len(failed) > 0 {
		return fail(fmt.Errorf("выгрузка не закончена (%s) — устрани причину и повтори tg rollback; старую сборку до этого не запускай", strings.Join(failed, ", ")))
	}
	fmt.Println("Готово: состояние выгружено для старой сборки, эта больше ничего не отправляет.\n" +
		`Теперь поставь старые exe в bin\ и запусти службу. Вернуться на эту сборку — scripts\build.ps1 (tg cutover снимет отметку).`)
	return 0
}

// rollbackBlockers — процессы, которые мешают выгрузке.
func rollbackBlockers(list []procs.Proc, self int) []string {
	var out []string
	for _, p := range list {
		if p.PID == self || cutoverKindOf(p) == cutoverSkip {
			continue
		}
		out = append(out, fmt.Sprintf("\n  %d (%s)", p.PID, describe(p)))
	}
	return out
}
