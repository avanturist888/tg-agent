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

const updatesUsage = `tg updates — карточки обновлений MAX и PyMax, которые ждут разбора.

  tg updates                        какие карточки открыты
  tg updates ack <источник|all> <вывод>
                                    разобрано, внимание владельца не нужно:
                                    снять кнопку и закрепление, дописать вывод
  tg updates escalate <источник> <что решить>
                                    нужно решение владельца: закрепить
                                    карточку со звуком и дописать, что решить`

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
			fmt.Println("Открытых карточек нет.")
		}
		for _, c := range cards {
			who := "закреплена для владельца"
			if c.Agent {
				who = "разбирает агент"
			}
			fmt.Printf("%-15s %s (сообщение %d, %s)\n", c.Source, c.Title, c.MessageID, who)
		}
		return 0
	case args[0] == "ack" && len(args) >= 3:
		var done []core.UpstreamCard
		err := service.Do(cctx, "upstream_ack", service.UpstreamAckArgs{
			Source: args[1], Note: strings.Join(args[2:], " "),
		}, &done)
		for _, c := range done {
			fmt.Printf("Разобрано: %s\n", c.Title)
		}
		if err != nil {
			return fail(err)
		}
		if len(done) == 0 {
			fmt.Println("Открытых карточек нет.")
		}
		return 0
	case args[0] == "escalate" && len(args) >= 3:
		var c core.UpstreamCard
		if err := service.Do(cctx, "upstream_escalate", service.UpstreamAckArgs{
			Source: args[1], Note: strings.Join(args[2:], " "),
		}, &c); err != nil {
			return fail(err)
		}
		fmt.Printf("Закреплена для владельца: %s\n", c.Title)
		return 0
	case args[0] == "-h" || args[0] == "--help" || args[0] == "help":
		fmt.Println(updatesUsage)
		return 0
	}
	fmt.Fprintln(os.Stderr, updatesUsage)
	return 2
}
