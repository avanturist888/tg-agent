package core

import (
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"tgagent/internal/outbox"
)

func TestMatchSent(t *testing.T) {
	sent := time.Date(2026, 11, 2, 9, 14, 42, 0, time.UTC)
	at := func(sec int) int { return int(sent.Add(time.Duration(sec) * time.Second).Unix()) }
	msgs := []tg.MessageClass{
		&tg.Message{ID: 10, Out: true, Date: at(-25), Message: "Добрый день! Смета готова"},
		&tg.Message{ID: 11, Out: false, Date: at(-20), Message: "Добрый день! Смета готова"},
		&tg.Message{ID: 12, Out: true, Date: at(-24), Message: "другое"},
		&tg.Message{ID: 9, Out: true, Date: at(-600), Message: "Добрый день! Смета готова"},
	}
	d := &outbox.Draft{Text: "Добрый день! **Смета** готова"}
	if got := matchSent(d, sent, false, msgs, map[int64]bool{}); got != 10 {
		t.Fatalf("по тексту: %d, ждали 10", got)
	}
	// уже связанное с другим черновиком не берём; другое своё в ту минуту — не оно
	if got := matchSent(d, sent, false, msgs, map[int64]bool{10: true}); got != 0 {
		t.Fatalf("неоднозначно: %d, ждали 0", got)
	}
	// одно своё сообщение в ту минуту — оно, даже если текст изменили ссылки
	one := []tg.MessageClass{&tg.Message{ID: 20, Out: true, Date: at(-30), Message: "Смотри сайт"}}
	if got := matchSent(&outbox.Draft{Text: "Смотри [сайт](https://x.ru)"}, sent, false, one, map[int64]bool{}); got != 20 {
		t.Fatalf("одно своё: %d, ждали 20", got)
	}
	// в Избранном своё без out
	self := []tg.MessageClass{&tg.Message{ID: 30, Date: at(-28), Message: "тест"}}
	if got := matchSent(&outbox.Draft{Text: "тест"}, sent, true, self, map[int64]bool{}); got != 30 {
		t.Fatalf("Избранное: %d, ждали 30", got)
	}
}
