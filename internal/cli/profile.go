package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"

	"tgagent/internal/config"
)

// cmdInit — заготовка именованного профиля: каталоги, .env с ключами
// приложения (те же, что у основного: одно приложение my.telegram.org годится
// для нескольких аккаунтов) и белый список с одним Избранным. Готовое не
// перезаписывает. Ни сессии, ни входа здесь нет — вход потом `tg login`.
func cmdInit(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	phone := fs.String("phone", "", "номер аккаунта профиля (+7…); можно ввести и при входе")
	if _, err := parse(fs, args); err != nil {
		return 2
	}
	prof := config.Profile()
	if prof == "" {
		fmt.Fprintln(os.Stderr, "init — для именованного профиля: tg --profile <имя> init (основной настраивается как раньше).")
		return 2
	}
	for _, dir := range []string{config.ConfigDir(), config.DataPath()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fail(err)
		}
	}
	tg := config.CLIHint() + " --profile " + prof
	created := []string{}
	if _, err := os.Stat(config.EnvPath()); errors.Is(err, os.ErrNotExist) {
		main, _ := godotenv.Read(filepath.Join(config.Root, ".env"))
		if main["TG_API_ID"] == "" || main["TG_API_HASH"] == "" {
			fmt.Fprintln(os.Stderr, "В .env основного профиля нет TG_API_ID / TG_API_HASH — впиши их в .env профиля сам (https://my.telegram.org).")
		}
		var b strings.Builder
		fmt.Fprintf(&b, "# Профиль %s: отдельный аккаунт Telegram. Команды — %s <команда>.\n", prof, tg)
		b.WriteString("# Ключи приложения те же, что у основного профиля: одно приложение\n")
		b.WriteString("# my.telegram.org годится для нескольких аккаунтов.\n")
		fmt.Fprintf(&b, "TG_API_ID=%s\nTG_API_HASH=%s\n", main["TG_API_ID"], main["TG_API_HASH"])
		if p := main["TG_PROXY"]; p != "" {
			fmt.Fprintf(&b, "TG_PROXY=%s\n", p)
		}
		b.WriteString("\n# Номер аккаунта профиля; пусто — login спросит.\n")
		fmt.Fprintf(&b, "TG_PHONE=%s\n", strings.TrimSpace(*phone))
		b.WriteString("\n# Подтверждение отправки. Бот основного профиля сюда не годится: его кнопки\n")
		b.WriteString("# слушает основная служба. Пока черновики подтверждаются командой\n")
		fmt.Fprintf(&b, "#   %s approve <id> --send   (или в окне: tgw.exe --profile %s gui)\n", tg, prof)
		b.WriteString("# Свои кнопки: создать бота у @BotFather, вписать TG_BOT_TOKEN и\n")
		b.WriteString("# TG_APPROVAL_CHAT_ID (свой id в Telegram) и поставить TG_SEND_POLICY=bot_approval.\n")
		b.WriteString("TG_SEND_POLICY=human_approval\n")
		if err := os.WriteFile(config.EnvPath(), []byte(b.String()), 0o600); err != nil {
			return fail(err)
		}
		created = append(created, config.EnvPath())
	}
	if _, err := os.Stat(config.ChatsFile()); errors.Is(err, os.ErrNotExist) {
		body := fmt.Sprintf(`# Белый список чатов профиля %s. Добавить чат:
#   %s dialogs --filter "часть названия"
#   %s allow <alias> --match "часть названия" [--send]

[[chat]]
alias = "saved"
id = "me"
title = "Избранное (%s)"
read = true
send = true
note = "Избранное аккаунта профиля — песочница для проверок"
`, prof, tg, tg, prof)
		if err := os.WriteFile(config.ChatsFile(), []byte(body), 0o600); err != nil {
			return fail(err)
		}
		created = append(created, config.ChatsFile())
	}
	if len(created) == 0 {
		fmt.Printf("Профиль %s уже заготовлен: %s\n", prof, config.Home())
	} else {
		fmt.Printf("Профиль %s заготовлен в %s:\n", prof, config.Home())
		for _, c := range created {
			fmt.Println("  +", c)
		}
	}
	fmt.Printf(`
Дальше:
  1. Вход (код придёт в Telegram аккаунта профиля):   %s login
  2. Служба профиля в автозапуск:   powershell -ExecutionPolicy Bypass -File %s -Profile %s
  3. MCP для агентов:   claude mcp add --scope user telegram-%s -- %s --profile %s mcp
  4. Проверка:   %s doctor
`, tg, filepath.Join(config.Root, "scripts", "install-task.ps1"), prof, prof,
		filepath.ToSlash(filepath.Join(config.Root, "bin", "tg.exe")), prof, tg)
	return 0
}
