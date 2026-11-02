package core

// Досвязка черновиков с их сообщениями. С 27.10 по 02.11.2026 отправка в
// Telegram теряла ответ (deliverTelegram присваивал его параметру), и
// черновики уходили в sent с message_id 0: tg_wait_approval отдавал 0, а
// tg_edit_message не находил своё сообщение. Служба при старте находит
// такие сообщения по истории чата — свои, отправленные в ту же минуту, с
// тем же текстом — и дописывает id в черновик.

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/gotd/td/tg"

	"tgagent/internal/config"
	"tgagent/internal/outbox"
	"tgagent/internal/tgc"
)

// repairSince — раньше этой даты id не терялся.
var repairSince = time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)

// RepairSentIDs — дописать message_id отправленным черновикам, у которых он 0.
// Что не нашлось однозначно, остаётся как есть (repair_sent_skipped в аудите).
func RepairSentIDs(ctx context.Context, s *config.Settings) {
	if s.IsMAX() {
		return
	}
	box := outbox.New(s.OutboxPath())
	drafts, err := box.List(outbox.Sent)
	if err != nil {
		return
	}
	taken := map[string]map[int64]bool{} // чат → id, уже связанные с черновиками
	type item struct {
		d  *outbox.Draft
		at time.Time
	}
	var todo []item
	for _, d := range drafts {
		if d.MessageID != nil && *d.MessageID > 0 {
			if taken[d.Chat] == nil {
				taken[d.Chat] = map[int64]bool{}
			}
			taken[d.Chat][*d.MessageID] = true
			continue
		}
		if d.SentAt == nil {
			continue
		}
		at, err := time.Parse(time.RFC3339, *d.SentAt)
		if err != nil || at.Before(repairSince) {
			continue
		}
		if d.EditOf != 0 { // правка: её сообщение — то, что правили
			setRepaired(s, box, d, d.EditOf)
			continue
		}
		todo = append(todo, item{d, at})
	}
	if len(todo) == 0 {
		return
	}
	// свежие первыми: их ещё можно править
	sort.Slice(todo, func(i, j int) bool { return todo[i].at.After(todo[j].at) })
	fixed, skipped := 0, 0
	err = tgc.Run(ctx, s, tgc.Opts{Wait: 60 * time.Second, Caller: "repair_sent"}, func(ctx context.Context, c *tgc.Conn) error {
		targets := map[string]tgc.Target{}
		for _, it := range todo {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			d := it.d
			t, ok := targets[d.Chat]
			if !ok {
				rule, err := s.Resolve(d.Chat)
				if err != nil {
					skipped++
					continue
				}
				if t, err = c.Resolve(ctx, rule); err != nil {
					skipped++
					continue
				}
				targets[d.Chat] = t
			}
			// часы этой машины спешат: время отправки по ним позже серверного
			b, err := c.HistoryAround(ctx, t, int(it.at.Unix()), 40)
			if err != nil {
				logEvent(s, "repair_sent_failed", "draft_id", d.ID, "chat", d.Chat, "error", err.Error())
				return err // FLOOD_WAIT и т.п. — продолжим при следующем старте
			}
			if taken[d.Chat] == nil {
				taken[d.Chat] = map[int64]bool{}
			}
			id := matchSent(d, it.at, t.Kind == "self", b.Messages, taken[d.Chat])
			if id == 0 {
				skipped++
				var first, last int
				if n := len(b.Messages); n > 0 {
					first, last = b.Messages[n-1].GetID(), b.Messages[0].GetID()
				}
				logEvent(s, "repair_sent_skipped", "draft_id", d.ID, "chat", d.Chat,
					"history", len(b.Messages), "oldest", first, "newest", last)
			} else {
				taken[d.Chat][id] = true
				setRepaired(s, box, d, id)
				fixed++
			}
			sleepCtx(ctx, 300*time.Millisecond)
		}
		return nil
	})
	if err != nil && ctx.Err() == nil {
		logEvent(s, "repair_sent_stopped", "error", err.Error())
	}
	logEvent(s, "repair_sent_done", "fixed", fixed, "skipped", skipped, "total", len(todo))
}

func setRepaired(s *config.Settings, box *outbox.Outbox, d *outbox.Draft, id int64) {
	_, err := box.Update(d.ID, func(d *outbox.Draft) error {
		d.MessageID = &id
		d.AddHistory("event", "message_id_repaired", "at", time.Now().UTC().Format(time.RFC3339), "message_id", id)
		return nil
	})
	if err == nil {
		logEvent(s, "repair_sent", "draft_id", d.ID, "chat", d.Chat, "message_id", id)
	}
}

// matchSent — сообщение черновика среди истории: своё, отправленное
// около sentAt и ещё ни с кем не связанное; из нескольких — с тем же текстом.
func matchSent(d *outbox.Draft, sentAt time.Time, self bool, msgs []tg.MessageClass, taken map[int64]bool) int64 {
	var cands []*tgc.MessageT
	for _, m := range msgs {
		msg, ok := m.(*tgc.MessageT)
		if !ok || (!msg.Out && !self) || taken[int64(msg.ID)] {
			continue
		}
		at := time.Unix(int64(msg.Date), 0)
		if at.Before(sentAt.Add(-3*time.Minute)) || at.After(sentAt.Add(2*time.Minute)) {
			continue
		}
		if d.Text == "" && msg.Media == nil {
			continue
		}
		cands = append(cands, msg)
	}
	want := letters(d.Text)
	var same []*tgc.MessageT
	for _, m := range cands {
		if d.Text != "" && sameText(want, letters(tgc.MessageText(m))) {
			same = append(same, m)
		}
	}
	switch {
	case len(same) >= 1:
		// одинаковый текст дважды — берём самое раннее несвязанное
		best := same[0]
		for _, m := range same[1:] {
			if m.ID < best.ID {
				best = m
			}
		}
		return int64(best.ID)
	case len(cands) == 1 && samePrefix(want, letters(tgc.MessageText(cands[0]))):
		// одно своё сообщение в ту минуту и начало то же — оно (ссылки меняют хвост)
		return int64(cands[0].ID)
	}
	return 0
}

// letters — только буквы и цифры в нижнем регистре: разметка markdown
// в отправленном превращается в оформление, текст остаётся.
func letters(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sameText — тексты совпадают; ссылки и упоминания могли поменять хвост,
// поэтому хватает совпадения первых 60 знаков (короткие — целиком).
func sameText(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ra, rb := []rune(a), []rune(b)
	if len(ra) < 60 || len(rb) < 60 {
		return a == b
	}
	return string(ra[:60]) == string(rb[:60])
}

// samePrefix — совпадают первые 20 знаков (или весь текст, если он короче);
// у файлов без текста сравнивать нечего.
func samePrefix(a, b string) bool {
	ra, rb := []rune(a), []rune(b)
	n := min(20, len(ra), len(rb))
	if len(ra) == 0 {
		return true
	}
	return n > 0 && string(ra[:n]) == string(rb[:n])
}
