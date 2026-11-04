// Package secret — секреты в тексте сообщений, которые не должны попасть
// агенту в контекст (а оттуда — в транскрипт сессии).
//
// Сейчас это токены ботов: ответ BotFather на /newbot и /revoke несёт
// действующий токен открытым текстом, а превью последнего сообщения чата
// видит любой агент, открывший tg_list_chats(with_status=true).
package secret

import "regexp"

// botToken — «<id бота>:<секрет>». Хвост короче настоящего (35 символов)
// тоже прячем: превью обрезает текст и может разрезать токен. Без \b:
// в JSON перед токеном бывает «\n», и граница слова не находится.
var botToken = regexp.MustCompile(`(\d{6,12}):[A-Za-z0-9_-]{12,}`)

// Mask заменяет секрет токена на пометку; id бота (не секрет) остаётся.
func Mask(s string) string {
	return botToken.ReplaceAllString(s, "${1}:[токен бота скрыт tg-agent]")
}

// BotTokens — все токены ботов в тексте, по порядку.
func BotTokens(s string) []string { return botToken.FindAllString(s, -1) }
