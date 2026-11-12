package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/procs"
	"tgagent/internal/store"
)

// cmdCutover — tg cutover: передать состояние от старой сборки новой.
// Его зовёт scripts\build.ps1 после замены exe, пока службы остановлены.
func cmdCutover(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("cutover", flag.ContinueOnError)
	wait := fs.Duration("wait", time.Minute, "сколько ждать, пока процессы прежних сборок закончат сами")
	if _, err := parse(fs, args); err != nil {
		return 2
	}
	var dirs []string
	for _, acct := range config.Accounts() {
		dirs = append(dirs, filepath.Join(config.HomeOf(acct), "data"))
	}
	c := cutover{bin: filepath.Join(config.Root, "bin"), dirs: dirs, wait: *wait, out: os.Stdout,
		list: procs.List, terminate: procs.Terminate, sleep: sleepCtx}
	if err := c.run(ctx); err != nil {
		return fail(err)
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// cutover — передача состояния. Старые файлы (до 12.11.2026) переносятся в
// state.db, только когда процессов старой сборки больше нет: они не знают
// о базе и писали бы мимо неё (store.CutoverFile). Поэтому — дождаться их
// (или завершить) и поставить отметку в data/ каждого профиля. После замены
// exe новые процессы запускаются уже из новой сборки, так что дождаться —
// значит дождаться всех, кто был до замены.
type cutover struct {
	bin       string   // bin\ установки
	dirs      []string // data/ профилей
	wait      time.Duration
	out       io.Writer
	list      func(bin string) ([]procs.Proc, error)
	terminate func(procs.Proc) error
	sleep     func(context.Context, time.Duration)
}

// cutoverKind — что делать с процессом установки.
type cutoverKind int

const (
	cutoverSkip    cutoverKind = iota // состояния не держит
	cutoverStop                       // живёт долго (служба, окно): ждать нечего
	cutoverWait                       // короткий (вызов инструмента, CLI): дать закончить
	cutoverUnknown                    // командная строка не читается
)

func cutoverKindOf(p procs.Proc) cutoverKind {
	if p.Args == nil {
		return cutoverUnknown
	}
	switch p.Verb() {
	case "mcp":
		// прослойка агента: состояния нет, вызовы уходят в службу или в
		// свежий bin\tg.exe — останавливать её значило бы выключить агенту
		// Telegram до новой сессии
		return cutoverSkip
	case "serve", "approvals", "gui", "watch":
		return cutoverStop
	}
	return cutoverWait
}

func (c cutover) run(ctx context.Context) error {
	var todo []string
	for _, d := range c.dirs {
		if len(store.LegacyPresent(d)) > 0 {
			todo = append(todo, d)
		}
	}
	if len(todo) == 0 {
		fmt.Fprintln(c.out, "Состояние уже в state.db — переносить нечего.")
		return nil
	}
	deadline := time.Now().Add(c.wait)
	told := map[int]bool{}
	var unknown []procs.Proc
	for {
		list, err := c.list(c.bin)
		if err != nil {
			return fmt.Errorf("процессы установки не перечислить: %w", err)
		}
		var waiting []procs.Proc
		unknown = unknown[:0]
		for _, p := range list {
			kind := cutoverKindOf(p)
			if kind == cutoverWait && !time.Now().Before(deadline) {
				kind = cutoverStop
			}
			switch kind {
			case cutoverStop:
				if err := c.terminate(p); err != nil {
					return fmt.Errorf("процесс %d (%s) не завершить: %w", p.PID, describe(p), err)
				}
				fmt.Fprintf(c.out, "Завершён процесс прежней сборки: %d (%s)\n", p.PID, describe(p))
				if p.Verb() == "gui" {
					fmt.Fprintln(c.out, "  окно управления закрыто — открой его заново: tgw gui")
				}
			case cutoverWait:
				waiting = append(waiting, p)
				if !told[p.PID] {
					told[p.PID] = true
					fmt.Fprintf(c.out, "Жду, пока закончит: %d (%s)\n", p.PID, describe(p))
				}
			case cutoverUnknown:
				unknown = append(unknown, p)
			}
		}
		if len(waiting) == 0 && (len(unknown) == 0 || !time.Now().Before(deadline)) {
			break
		}
		c.sleep(ctx, time.Second)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if len(unknown) > 0 {
		var b strings.Builder
		for _, p := range unknown {
			fmt.Fprintf(&b, "\n  %d %s", p.PID, p.Exe)
		}
		return errors.New("не разобрать, что делают процессы установки (командная строка не читается):" + b.String() +
			"\nОстанови их и повтори tg cutover — до тех пор старые файлы не переносятся")
	}
	for _, d := range todo {
		if err := store.MarkCutover(d, config.Build); err != nil {
			return fmt.Errorf("%s: отметка обновления: %w", d, err)
		}
	}
	fmt.Fprintf(c.out, "Процессов прежней сборки нет — старое состояние перенесётся в state.db при запуске службы (%d профил%s).\n",
		len(todo), plural(len(todo), "ь", "я", "ей"))
	return nil
}

func describe(p procs.Proc) string {
	name := filepath.Base(p.Exe)
	if v := p.Verb(); v != "" {
		name += " " + v
	}
	if prof := p.Profile(); prof != "" {
		name += " --profile " + prof
	}
	return name
}

func plural(n int, one, few, many string) string {
	switch {
	case n%10 == 1 && n%100 != 11:
		return one
	case n%10 >= 2 && n%10 <= 4 && (n%100 < 10 || n%100 >= 20):
		return few
	}
	return many
}
