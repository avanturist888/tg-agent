package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"tgagent/internal/audit"
	"tgagent/internal/config"
)

// Надзор за службой.
//
// Упавший процесс службы (паника в gotd или у нас) Планировщик сам не
// поднимает, а трассировка паники у задачи без консоли пропадает. Поэтому
// `tgw serve` — лишь надзиратель: запускает рабочий процесс (`serve --worker`),
// пишет его вывод в data/service.log и поднимает заново, если тот упал.

// WorkerArg — рабочий процесс службы (его запускает надзиратель).
const WorkerArg = "--worker"

const logLimit = 5 << 20 // service.log больше — начинаем новый, старый в .1

// LogPath — журнал рабочего процесса службы (там и трассировки паник).
func LogPath(s *config.Settings) string { return filepath.Join(s.DataDir(), "service.log") }

// Supervise — держать рабочий процесс службы запущенным, пока не отменят ctx.
func Supervise(ctx context.Context, loader func() (*config.Settings, error)) error {
	s, err := loader()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	job, err := newKillJob() // рабочий умирает вместе с надзирателем
	if err != nil {
		return err
	}
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		started := time.Now()
		code, runErr := runWorker(ctx, s, exe, job)
		if ctx.Err() != nil {
			return nil
		}
		why := fmt.Sprint(runErr)
		if runErr == nil {
			why = fmt.Sprintf("код выхода %d", code)
		}
		_ = audit.Log(s.AuditPath(), "service_crashed", "reason", why, "last", lastLines(LogPath(s), 6), "log", LogPath(s))
		if time.Since(started) > 5*time.Minute {
			backoff = 2 * time.Second // долго работал — сбой разовый
		}
		sleep(ctx, backoff)
		backoff = min(backoff*2, time.Minute)
	}
	return nil
}

func runWorker(ctx context.Context, s *config.Settings, exe string, job killJob) (int, error) {
	path := LogPath(s)
	if st, err := os.Stat(path); err == nil && st.Size() > logLimit {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fmt.Fprintf(f, "\n=== %s: запуск рабочего процесса, сборка %s\n", time.Now().Format("2006-01-02 15:04:05"), config.Build)

	cmd := exec.CommandContext(ctx, exe, "serve", WorkerArg)
	cmd.Dir = config.Root
	cmd.Stdout, cmd.Stderr = f, f
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	if err := job.add(cmd.Process); err != nil {
		fmt.Fprintf(f, "не удалось привязать рабочий процесс к надзирателю: %v\n", err)
	}
	err = cmd.Wait()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// lastLines — хвост журнала (первые строки паники) — для аудита.
func lastLines(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	panicAt := -1
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "=== ") {
			lines, panicAt = lines[:0], -1 // только последний запуск
			continue
		}
		if panicAt < 0 && (strings.HasPrefix(line, "panic:") || strings.HasPrefix(line, "fatal error:")) {
			panicAt = len(lines)
		}
		lines = append(lines, line)
	}
	if panicAt >= 0 {
		lines = lines[panicAt:] // начало паники важнее конца трассировки
	} else if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
