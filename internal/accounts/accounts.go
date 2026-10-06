// Package accounts — вызов операций в службе другого аккаунта (профиля).
//
// У каждого аккаунта своя служба со своим соединением с Telegram; один
// процесс за два аккаунта не работает (профиль — глобальное состояние).
// Поэтому вызов к чужому аккаунту уходит в его службу по её каналу, а если
// её нет — в свежий `bin\tg.exe --profile <аккаунт> op <операция>`, который
// выполнит ту же операцию на месте.
package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"tgagent/internal/config"
	"tgagent/internal/svc"
)

// Reply — ответ `tg op`: результат или ошибка того же вида, что из службы.
type Reply struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *svc.Error      `json:"error,omitempty"`
}

// Service — операция только в запущенной службе аккаунта (svc.Unavailable — её нет).
func Service(ctx context.Context, account, op string, args, out any) error {
	_, err := svc.CallAt(ctx, svc.AddressOf(config.HomeOf(account)), op, args, out)
	return err
}

// Call — операция в службе аккаунта, а без неё — в дочернем процессе.
func Call(ctx context.Context, account, op string, args, out any) error {
	err := Service(ctx, account, op, args, out)
	if !errors.Is(err, svc.Unavailable) {
		return err
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, exe(), "--profile", account, "op", op)
	hideWindow(cmd)
	cmd.Stdin = bytes.NewReader(raw)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var rep Reply
	if jerr := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &rep); jerr != nil {
		msg := bytes.TrimSpace(stderr.Bytes())
		if len(msg) > 500 {
			msg = msg[len(msg)-500:]
		}
		return fmt.Errorf("аккаунт %s: служба не запущена, а tg op %s не ответил: %v %s", account, op, runErr, msg)
	}
	if rep.Error != nil {
		return rep.Error
	}
	if out != nil && len(rep.Result) > 0 {
		return json.Unmarshal(rep.Result, out)
	}
	return nil
}

// exe — bin\tg.exe установки (консольный: его вывод читаем).
func exe() string {
	p := filepath.Join(config.Root, "bin", "tg.exe")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	self, _ := os.Executable()
	return self
}
