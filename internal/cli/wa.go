package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"tgagent/internal/config"
	"tgagent/internal/core"
	"tgagent/internal/service"
	"tgagent/internal/wac"
)

const waUsage = `tg wa — аккаунт WhatsApp (шлюз — связанное устройство, как WhatsApp Web).

  tg wa login [--phone +79991234567] [--force]
                       связать устройство: QR сканируют в WhatsApp на телефоне
                       (Настройки → Связанные устройства → Привязка устройства);
                       с --phone — вместо QR код из 8 знаков, его вводят там же
                       («Привязать по номеру телефона»)
  tg wa status         жив ли вход: номер, имя, сколько чатов видел шлюз

Устройство — data\wa\device.db: это полный доступ к аккаунту, никому не давать.
Сообщения — data\wa\messages.db (истории на сервере WhatsApp нет, шлюз хранит её сам).`

func cmdWA(ctx context.Context, args []string) int {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "login":
		return waLogin(ctx, args)
	case "status", "":
		return waStatus(ctx)
	case "-h", "--help", "help":
		fmt.Println(waUsage)
		return 0
	}
	fmt.Fprintln(os.Stderr, waUsage)
	return 2
}

func waLogin(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("wa login", flag.ContinueOnError)
	force := fs.Bool("force", false, "связать заново, даже если устройство уже связано")
	phone := fs.String("phone", "", "номер телефона: вход кодом вместо QR")
	if _, err := parse(fs, args); err != nil {
		return 2
	}
	if config.Network() != config.NetWA {
		fmt.Fprintf(os.Stderr, "Профиль %q — не WhatsApp (в его .env нужно TG_NETWORK=whatsapp).\n", config.AccountName())
		return 2
	}
	// вход идёт в самой службе: её соединение примет и историю с телефона
	if !service.Up(ctx) {
		return fail(core.ErrNoWA)
	}
	num := strings.TrimLeft(strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(*phone), "+")
	var st core.WALogin
	if err := service.Do(ctx, "wa_login", service.WALoginArgs{Phone: num, Force: *force}, &st); err != nil {
		return fail(err)
	}
	if st.Stage == "done" && !*force {
		fmt.Printf("Устройство уже связано: +%s. Связать заново — tg wa login --force.\n", st.Phone)
		return 0
	}
	qrPath := filepath.Join(core.WADir(), "qr.png")
	defer os.Remove(qrPath)
	shown := ""
	deadline := time.Now().Add(11 * time.Minute)
	for time.Now().Before(deadline) {
		switch st.Stage {
		case "qr":
			if st.QR != shown {
				shown = st.QR
				if err := showWAQR(st.QR, qrPath, time.Until(st.Until)); err != nil {
					return fail(err)
				}
			}
		case "code":
			if st.Code != shown {
				shown = st.Code
				fmt.Printf("\nКод для входа: %s\nНа телефоне: WhatsApp → Связанные устройства → Привязка устройства → "+
					"«Привязать по номеру телефона» — и введи этот код.\n", st.Code)
			}
		case "done":
			fmt.Printf("\nWhatsApp связан: +%s. Служба уже на связи и принимает историю чатов с телефона.\n", st.Phone)
			return 0
		case "failed":
			return fail(errors.New(st.Error))
		}
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(time.Second):
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := service.Do(cctx, "wa_login_state", nil, &st)
		cancel()
		if err != nil {
			return fail(err)
		}
	}
	return fail(errors.New("за 10 минут вход не подтвердили"))
}

// showWAQR — QR в консоли и картинкой рядом с устройством.
func showWAQR(code, png string, valid time.Duration) error {
	q, err := qrcode.New(code, qrcode.Medium)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(png), 0o700); err != nil {
		return err
	}
	if err := q.WriteFile(320, png); err != nil {
		return err
	}
	fmt.Println(q.ToSmallString(false))
	fmt.Printf("Отсканируй QR в WhatsApp на телефоне: Настройки → Связанные устройства → Привязка устройства.\n"+
		"Плохо видно — открой картинку: %s\nКод действует ещё %d с, потом появится новый.\n", png, int(valid.Seconds()))
	return nil
}

func waStatus(ctx context.Context) int {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var st core.WAStatus
	err := service.Do(cctx, "wa_status", nil, &st)
	switch {
	case err == nil:
		state := "на связи"
		if !st.Connected {
			state = "нет связи (переподключается)"
		}
		fmt.Printf("WhatsApp: %s (+%s) — служба %s, чатов в хранилище %d.\n", st.Name, st.Phone, state, st.Chats)
		return 0
	case wac.HasSession(cctx, core.WADir()):
		fmt.Println("Устройство связано, но служба профиля сейчас не держит соединение:", err)
		return 1
	}
	fmt.Println("Входа в WhatsApp нет. Связать устройство: tg --profile " + config.Profile() + " wa login")
	return 1
}
