package tgc

import (
	"context"
	"fmt"
	"strings"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"tgagent/internal/audit"
	"tgagent/internal/config"
)

// floodLog — каждый FLOOD_WAIT в аудит с методом: без него не понять, какой
// запрос упёрся в лимит Telegram (ошибка наверху метода не называет).
func floodLog(s *config.Settings) telegram.Middleware {
	return telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
			err := next.Invoke(ctx, input, output)
			if wait, ok := tgerr.AsFloodWait(err); ok {
				method := strings.TrimPrefix(fmt.Sprintf("%T", input), "*tg.")
				_ = audit.Log(s.AuditPath(), "flood_wait", "method", method, "seconds", int(wait.Seconds()))
			}
			return err
		}
	})
}
