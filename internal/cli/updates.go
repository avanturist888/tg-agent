package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"tgagent/internal/core"
	"tgagent/internal/service"
)

const updatesUsage = `tg updates — закреплённые карточки обновлений MAX и PyMax.

  tg updates                        какие карточки ждут разбора
  tg updates ack <источник|all> <вывод>
                                    разобрано: открепить, снять кнопку и
                                    дописать в карточку вывод — для изменений,
                                    которые не требуют внимания владельца`

func cmdUpdates(ctx context.Context, args []string) int {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	switch {
	case len(args) == 0:
		var cards []core.UpstreamCard
		if err := service.Do(cctx, "upstream_cards", nil, &cards); err != nil {
			return fail(err)
		}
		if len(cards) == 0 {
			fmt.Println("Закреплённых карточек нет.")
		}
		for _, c := range cards {
			fmt.Printf("%-15s %s (сообщение %d)\n", c.Source, c.Title, c.MessageID)
		}
		return 0
	case args[0] == "ack" && len(args) >= 3:
		var done []core.UpstreamCard
		err := service.Do(cctx, "upstream_ack", service.UpstreamAckArgs{
			Source: args[1], Note: strings.Join(args[2:], " "),
		}, &done)
		for _, c := range done {
			fmt.Printf("Откреплена: %s\n", c.Title)
		}
		if err != nil {
			return fail(err)
		}
		if len(done) == 0 {
			fmt.Println("Закреплённых карточек нет.")
		}
		return 0
	case args[0] == "-h" || args[0] == "--help" || args[0] == "help":
		fmt.Println(updatesUsage)
		return 0
	}
	fmt.Fprintln(os.Stderr, updatesUsage)
	return 2
}
