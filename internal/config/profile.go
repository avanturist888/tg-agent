package config

// Профили — несколько аккаунтов Telegram на одной установке.
//
// Основной профиль — как всегда: .env, config/ и data/ в корне установки.
// Именованный профиль (tg --profile work … или TG_PROFILE=work) живёт в
// profiles/<имя>/ со своими .env, config/ (белый список, t3.toml,
// agents.toml) и data/ (сессия MTProto, черновики, аудит, токены T3, треды
// шлюза). Общие у профилей только программа (bin/) и AGENTS.md.
//
// У каждого профиля своя служба (задача Планировщика tg-agent-<имя>) со
// своим именованным каналом: имя канала считается от каталога профиля.
// Бот подтверждений: апдейты одного бота может слушать только одна служба.
// Профиль без своего бота (TG_SEND_POLICY=bot_approval, TG_BOT_TOKEN пуст)
// берёт бот основного профиля (Settings.SharedBot): карточки шлёт сам, а
// нажатия принимает основная служба и по метке в callback_data передаёт его
// службе. Чужой config.env (cc-telegram-notify) профиль не берёт.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProfileEnv — переменная окружения с именем профиля.
const ProfileEnv = "TG_PROFILE"

var profile = normProfile(os.Getenv(ProfileEnv))

func normProfile(name string) string {
	name = strings.TrimSpace(name)
	if strings.EqualFold(name, "default") || strings.EqualFold(name, MainAccount) {
		return ""
	}
	return name
}

// Profile — имя активного профиля ("" — основной).
func Profile() string { return profile }

// SetProfile — выбрать профиль (флаг --profile). Имя уходит и в окружение:
// дочерние процессы (рабочий процесс службы, tg tool-call, send-due)
// работают в том же профиле.
func SetProfile(name string) error {
	name = normProfile(name)
	if name != "" && !safeName(name) {
		return &Error{Msg: fmt.Sprintf("Имя профиля %q: только латиница, цифры, - и _.", name)}
	}
	profile = name
	if name == "" {
		return os.Unsetenv(ProfileEnv)
	}
	return os.Setenv(ProfileEnv, name)
}

// ProfileName — имя для людей.
func ProfileName() string {
	if profile == "" {
		return "основной"
	}
	return profile
}

// Home — каталог профиля: там .env, config/ и data/.
func Home() string {
	if profile == "" {
		return Root
	}
	return filepath.Join(Root, "profiles", profile)
}

// DataPath — data/ профиля: сессия, черновики, аудит, ленты.
func DataPath() string { return filepath.Join(Home(), "data") }

// ConfigDir — config/ профиля: белый список и прочие настройки.
func ConfigDir() string { return filepath.Join(Home(), "config") }

// EnvPath — .env профиля.
func EnvPath() string { return filepath.Join(Home(), ".env") }

// ChatsFile — белый список профиля.
func ChatsFile() string { return filepath.Join(ConfigDir(), "chats.toml") }
