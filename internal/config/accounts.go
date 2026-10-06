package config

// Аккаунты — профили установки с точки зрения агента: «main» (основной) и
// именованные из profiles/. Пока аккаунт один, агент его не замечает. Когда
// их несколько, MCP-сервер любого из них видит все: чаты чужого аккаунта —
// по ссылке «<аккаунт>/<alias>», вызов уходит в службу того аккаунта
// (mcpserver/route.go).

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// MainAccount — имя основного профиля для агентов.
const MainAccount = "main"

// AccountName — имя текущего аккаунта.
func AccountName() string {
	if profile == "" {
		return MainAccount
	}
	return profile
}

// IsOwnAccount — имя текущего аккаунта ("" — тоже он).
func IsOwnAccount(name string) bool {
	return name == "" || strings.EqualFold(name, AccountName())
}

// HomeOf — каталог аккаунта: .env, config/ и data/.
func HomeOf(account string) string {
	if account == "" || strings.EqualFold(account, MainAccount) {
		return Root
	}
	return filepath.Join(Root, "profiles", account)
}

// ChatsFileOf — белый список аккаунта.
func ChatsFileOf(account string) string {
	return filepath.Join(HomeOf(account), "config", "chats.toml")
}

// Accounts — все аккаунты установки: main и профили, у которых есть .env.
func Accounts() []string {
	out := []string{MainAccount}
	entries, _ := os.ReadDir(filepath.Join(Root, "profiles"))
	var named []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !safeName(name) || normProfile(name) == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(Root, "profiles", name, ".env")); err == nil {
			named = append(named, name)
		}
	}
	sort.Strings(named)
	return append(out, named...)
}

// Multi — аккаунтов больше одного: агенту показываем, где чей чат.
func Multi() bool { return len(Accounts()) > 1 }

// FindAccount — каноническое имя аккаунта (без учёта регистра); "" — нет такого.
func FindAccount(name string) string {
	for _, a := range Accounts() {
		if strings.EqualFold(a, strings.TrimSpace(name)) {
			return a
		}
	}
	return ""
}

// SplitRef — «work/team» → ("work", "team"), если work — аккаунт установки.
func SplitRef(ref string) (account, alias string, ok bool) {
	head, rest, found := strings.Cut(strings.TrimSpace(ref), "/")
	if !found || rest == "" {
		return "", ref, false
	}
	if a := FindAccount(head); a != "" {
		return a, rest, true
	}
	return "", ref, false
}

// Ref — как агенту называть чат этого аккаунта: у основного — alias, у
// именованного — «<профиль>/<alias>» (так ссылка годится из любого аккаунта).
func (s *Settings) Ref(alias string) string {
	if profile == "" || alias == "" {
		return alias
	}
	return profile + "/" + alias
}

// FindChat — чат в списке по alias, id или username.
func FindChat(rules map[string]ChatRule, order []string, chat string) (ChatRule, bool) {
	low := strings.ToLower(strings.TrimSpace(chat))
	if low == "" {
		return ChatRule{}, false
	}
	for _, a := range order {
		if strings.ToLower(a) == low {
			return rules[a], true
		}
	}
	for _, a := range order {
		rule := rules[a]
		peer := strings.ToLower(rule.PeerString())
		if low == peer || strings.TrimLeft(low, "@") == strings.TrimLeft(peer, "@") {
			return rule, true
		}
	}
	return ChatRule{}, false
}

// SharesMainBot — аккаунт подтверждает отправку ботом основного профиля:
// своего бота у него нет. Карточки шлёт он сам, а нажатия принимает
// основная служба и передаёт ему.
func SharesMainBot(account string) bool {
	if strings.EqualFold(account, MainAccount) {
		return false
	}
	values, err := godotenv.Read(filepath.Join(HomeOf(account), ".env"))
	if err != nil {
		return false
	}
	policy := strings.ToLower(strings.TrimSpace(values["TG_SEND_POLICY"]))
	if policy != "" && policy != "bot_approval" {
		return false
	}
	tok := strings.TrimSpace(values["TG_BOT_TOKEN"])
	return tok == "" || tok == mainBotToken()
}

// CallbackPrefix — метка аккаунта в callback_data кнопок: у общего бота
// нажатие приходит основной службе, и по метке она отдаёт его нужной.
func (s *Settings) CallbackPrefix() string {
	if !s.SharedBot {
		return ""
	}
	return CallbackPrefixFor(profile)
}

// CallbackPrefixFor — метка кнопок аккаунта в общем боте ("" — основной).
func CallbackPrefixFor(account string) string {
	if account == "" || strings.EqualFold(account, MainAccount) {
		return ""
	}
	return "@" + account + "|"
}

// SplitCallback — «@work|d:…» → ("work", "d:…"); без метки — ("", data).
func SplitCallback(data string) (account, rest string) {
	if !strings.HasPrefix(data, "@") {
		return "", data
	}
	head, rest, ok := strings.Cut(data[1:], "|")
	if !ok {
		return "", data
	}
	return head, rest
}

// MainHasBot — у основного профиля есть бот подтверждений.
func MainHasBot() bool {
	tok, chat := mainBot()
	return tok != "" && chat != 0
}

// mainBot — бот и чат подтверждений основного профиля (из его .env или
// донора), не трогая окружение процесса.
func mainBot() (string, int64) {
	values, _ := godotenv.Read(filepath.Join(Root, ".env"))
	token := strings.TrimSpace(values["TG_BOT_TOKEN"])
	chat := strings.TrimSpace(values["TG_APPROVAL_CHAT_ID"])
	if token == "" || chat == "" {
		donor := values["TG_BOT_ENV_FILE"]
		if donor == "" {
			donor = defaultDonor()
		}
		d, _ := godotenv.Read(donor)
		if token == "" {
			token = strings.TrimSpace(d["NOTIFICATIONS_BOT_TOKEN"])
		}
		if chat == "" {
			chat = strings.TrimSpace(d["NOTIFICATIONS_CHAT_ID"])
		}
	}
	id, _ := strconv.ParseInt(chat, 10, 64)
	return token, id
}
