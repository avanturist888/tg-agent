package core

import (
	"context"
	"fmt"

	"tgagent/internal/config"
	"tgagent/internal/omap"
	"tgagent/internal/secret"
)

// LastBotToken — самый свежий токен бота в последних сообщениях чата (ответ
// BotFather). Агенту через инструменты токены не уходят (secret.Mask) —
// это для владельца и CLI: `tg bot-token botfather | <куда положить>`, чтобы
// токен шёл трубой прямо в настройки и не попадал ни в чей вывод.
func LastBotToken(ctx context.Context, s *config.Settings, chat string, limit int) (string, int, error) {
	if s.Agent != nil {
		return "", 0, denied("Агенту из контейнера токены ботов не выдаются.")
	}
	if limit <= 0 {
		limit = 10
	}
	res, err := ReadChat(ctx, s, chat, ReadOpts{Limit: limit})
	if err != nil {
		return "", 0, err
	}
	v, _ := res.Get("messages")
	list, _ := v.([]*omap.Map)
	for i := len(list) - 1; i >= 0; i-- {
		t, _ := list[i].Get("text")
		if toks := secret.BotTokens(fmt.Sprint(t)); len(toks) > 0 {
			id, _ := list[i].Get("id")
			n, _ := id.(int)
			logEvent(s, "bot_token_extracted", "chat", chat, "message_id", id)
			return toks[len(toks)-1], n, nil
		}
	}
	return "", 0, &NotFound{Msg: fmt.Sprintf("В последних %d сообщениях «%s» токена бота нет.", limit, chat)}
}
