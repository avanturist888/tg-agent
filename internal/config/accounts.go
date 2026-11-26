package config

// Аккаунты — профили установки с точки зрения агента: «main» (основной) и
// именованные из profiles/. Пока аккаунт один, агент его не замечает. Когда
// их несколько, MCP-сервер любого из них видит все: чаты чужого аккаунта —
// по ссылке «<аккаунт>/<alias>», вызов уходит в службу того аккаунта
// (mcpserver/route.go).

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/joho/godotenv"

	"tgagent/internal/store"
)

// NormPhone оставляет в номере телефона только ASCII-цифры '0'..'9'.
func NormPhone(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= '0' && c <= '9' {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// AccountMeta — сведения об аккаунте профиля (kv account в data/state.db).
type AccountMeta struct {
	ID        int64  `json:"id"`
	Phone     string `json:"phone,omitempty"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

// Label — имя и @username для показа людям и агентам.
func (u AccountMeta) Label() string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if u.Username != "" {
		if name != "" {
			name += " "
		}
		name += "@" + u.Username
	}
	return name
}

// AccountMetaOf читает kv account из data/state.db аккаунта, не создавая
// пустую базу на диске, если её ещё нет.
func AccountMetaOf(account string) (AccountMeta, bool) {
	dir := filepath.Join(HomeOf(account), "data")
	if _, err := os.Stat(filepath.Join(dir, store.FileName)); err != nil {
		return AccountMeta{}, false
	}
	db, err := store.Open(dir)
	if err != nil {
		return AccountMeta{}, false
	}
	var meta AccountMeta
	ok, err := store.GetKV(context.Background(), db.Q(), store.KeyAccount, &meta)
	if err != nil || !ok {
		return AccountMeta{}, false
	}
	return meta, true
}

// AccountPhone — нормализованный номер телефона аккаунта: сначала актуальный
// phone из state.db (kv account, обновляется при входе/подключении), если
// пусто — TG_PHONE из .env профиля.
func AccountPhone(account string) string {
	if meta, ok := AccountMetaOf(account); ok {
		if p := NormPhone(meta.Phone); p != "" {
			return p
		}
	}
	if values, err := godotenv.Read(filepath.Join(HomeOf(account), ".env")); err == nil {
		if p := NormPhone(values["TG_PHONE"]); p != "" {
			return p
		}
	}
	return ""
}

// AccountSelfID — числовой ID аккаунта из state.db (kv account); 0 — ещё не знаем.
func AccountSelfID(account string) int64 {
	if meta, ok := AccountMetaOf(account); ok {
		return meta.ID
	}
	return 0
}

// AccountNetwork — сеть аккаунта из TG_NETWORK в его .env (по умолчанию telegram).
func AccountNetwork(account string) string {
	values, err := godotenv.Read(filepath.Join(HomeOf(account), ".env"))
	if err != nil {
		return NetTelegram
	}
	return normNetwork(values["TG_NETWORK"])
}

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

// BotOf — разрешённые бот, чат подтверждений и признак общего бота для аккаунта.
func BotOf(account string) (token string, chatID int64, shared bool) {
	if account == "" || strings.EqualFold(account, MainAccount) {
		tok, chat := mainBot()
		return tok, chat, false
	}
	values, err := godotenv.Read(filepath.Join(HomeOf(account), ".env"))
	if err != nil {
		return "", 0, false
	}
	policy := normPolicy(values["TG_SEND_POLICY"])
	network := normNetwork(values["TG_NETWORK"])
	phone := AccountPhone(account)
	tok, chat := envBotCredentials(values)
	return resolveBot(account, policy, network, phone, tok, chat)
}

// ApprovalChatIDOf — разрешённый chat_id подтверждений аккаунта; 0 — не задан.
func ApprovalChatIDOf(account string) int64 {
	_, chatID, _ := BotOf(account)
	return chatID
}

// SharesBotWith — аккаунт использует общий бот, которым владеет hostAccount:
// карточки шлёт сам, а нажатия принимает служба hostAccount и передаёт ему.
func SharesBotWith(account, hostAccount string) bool {
	if strings.EqualFold(account, hostAccount) {
		return false
	}
	if accountPolicy(account) != "bot_approval" {
		return false
	}
	tok, _, shared := BotOf(account)
	if !shared || tok == "" {
		return false
	}
	hostTok, _, hostShared := BotOf(hostAccount)
	return !hostShared && hostTok != "" && tok == hostTok
}

// SharesMainBot — аккаунт подтверждает отправку ботом основного профиля.
func SharesMainBot(account string) bool {
	return SharesBotWith(account, MainAccount)
}

func normPolicy(raw string) string {
	p := strings.ToLower(strings.TrimSpace(raw))
	if p == "" {
		return "bot_approval"
	}
	return p
}

func accountPolicy(account string) string {
	values, err := godotenv.Read(filepath.Join(HomeOf(account), ".env"))
	if err != nil {
		return ""
	}
	return normPolicy(values["TG_SEND_POLICY"])
}

func envBotCredentials(values map[string]string) (string, int64) {
	token := strings.TrimSpace(values["TG_BOT_TOKEN"])
	chat := strings.TrimSpace(values["TG_APPROVAL_CHAT_ID"])
	if (token == "" || chat == "") && strings.TrimSpace(values["TG_BOT_ENV_FILE"]) != "" {
		d, _ := godotenv.Read(strings.TrimSpace(values["TG_BOT_ENV_FILE"]))
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

// resolveBot — единая логика разрешения (token, chatID, shared) для именованного
// профиля: явный TG_APPROVAL_CHAT_ID никогда не затирается; не-Telegram профили
// сначала сопоставляются по номеру с Telegram-профилем (без рекурсии); Telegram-
// профиль без явного chat_id берёт свой SelfID; при отсутствии своего бота —
// fallback на бот основного профиля.
func resolveBot(acct, policy, network, phone, token string, chatID int64) (string, int64, bool) {
	if acct == "" || strings.EqualFold(acct, MainAccount) || policy != "bot_approval" {
		return token, chatID, false
	}
	shared := false
	if network != NetTelegram && (token == "" || chatID == 0) {
		if tgAcct := findTelegramByPhone(phone); tgAcct != "" {
			tgTok, tgChat, _ := BotOf(tgAcct)
			if token == "" && tgTok != "" {
				token = tgTok
				shared = true
			} else if token != "" && token == tgTok {
				shared = true
			}
			if chatID == 0 && tgChat != 0 {
				chatID = tgChat
			} else if chatID == 0 {
				if selfID := AccountSelfID(tgAcct); selfID > 0 {
					chatID = selfID
				}
			}
		}
	} else if network == NetTelegram && chatID == 0 {
		if selfID := AccountSelfID(acct); selfID > 0 {
			chatID = selfID
		}
	}
	mainTok, mainChat := mainBot()
	if mainTok != "" && (token == "" || token == mainTok) {
		token = mainTok
		shared = true
		if chatID == 0 {
			chatID = mainChat
		}
	} else if !shared && token != "" && botTokenOwnedByOther(acct, token) {
		shared = true
	}
	return token, chatID, shared
}

// botTokenOwnedByOther — этот токен уже принадлежит main или другому профилю
// (Telegram-профилю приоритетно, либо первому в Accounts()): второй
// getUpdates на том же токене сорвал бы первый.
func botTokenOwnedByOther(acct, token string) bool {
	if token == "" {
		return false
	}
	if mainTok, _ := mainBot(); mainTok != "" && token == mainTok {
		return !strings.EqualFold(acct, MainAccount)
	}
	var firstTG, firstAny string
	for _, cand := range Accounts() {
		if strings.EqualFold(cand, MainAccount) {
			continue
		}
		values, err := godotenv.Read(filepath.Join(HomeOf(cand), ".env"))
		if err != nil {
			continue
		}
		if normPolicy(values["TG_SEND_POLICY"]) != "bot_approval" {
			continue
		}
		tok, _ := envBotCredentials(values)
		if tok != token {
			continue
		}
		if firstAny == "" {
			firstAny = cand
		}
		if firstTG == "" && normNetwork(values["TG_NETWORK"]) == NetTelegram {
			firstTG = cand
		}
	}
	owner := firstTG
	if owner == "" {
		owner = firstAny
	}
	return owner != "" && !strings.EqualFold(owner, acct)
}

// findTelegramByPhone ищет Telegram-профиль с тем же нормализованным номером:
// сначала main, затем именованные профили по порядку Accounts().
func findTelegramByPhone(phone string) string {
	phone = NormPhone(phone)
	if phone == "" {
		return ""
	}
	for _, cand := range Accounts() {
		if AccountNetwork(cand) != NetTelegram {
			continue
		}
		if AccountPhone(cand) == phone {
			return cand
		}
	}
	return ""
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
	if id == 0 {
		id = AccountSelfID(MainAccount)
	}
	return token, id
}
