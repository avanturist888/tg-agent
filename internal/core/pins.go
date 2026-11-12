package core

// Закреплённые карточки черновиков: карточка, которая ждёт нажатия владельца
// (ручное подтверждение), закреплена в чате с ботом, чтобы не потеряться среди
// карточек автоотправки, — без владельца такое сообщение не уйдёт. Решили
// (кнопкой, мимо кнопок, протухло) — откреплена. Какие закреплены —
// таблица pinned_drafts (черновик → карточка).

import (
	"context"
	"sync/atomic"
	"time"

	"tgagent/internal/bot"
	"tgagent/internal/config"
	"tgagent/internal/outbox"
	"tgagent/internal/store"
)

var (
	pinBot   = bot.Call // тесты подменяют
	sweeping atomic.Bool
)

// pinsTab — закреплённые карточки: черновик → id карточки.
var pinsTab = store.NewTable[int64](store.TPinnedDrafts)

// editPins — прочитать, поменять f и записать одной транзакцией.
func editPins(s *config.Settings, f func(map[string]int64)) error {
	db, err := stateDB(s)
	if err != nil {
		return err
	}
	return pinsTab.Edit(context.Background(), db, func(m map[string]int64) bool { f(m); return true })
}

// pinDraftCard — закрепить карточку, которая ждёт владельца.
func pinDraftCard(ctx context.Context, s *config.Settings, draftID string, cardID int64) {
	// уведомление уже было от самой карточки — второе («закрепил») ни к чему
	if err := pinBot(ctx, s, "pinChatMessage", map[string]any{
		"chat_id": s.ApprovalChatID, "message_id": cardID, "disable_notification": true,
	}, nil); err != nil {
		logEvent(s, "card_pin_failed", "draft_id", draftID, "message_id", cardID, "error", err.Error())
		return
	}
	if err := editPins(s, func(m map[string]int64) { m[draftID] = cardID }); err != nil {
		logEvent(s, "card_pin_failed", "draft_id", draftID, "error", err.Error())
	}
}

// unpinDraftCard — черновик решён: снять закрепление, если оно было.
func unpinDraftCard(ctx context.Context, s *config.Settings, draftID string) {
	var cardID int64
	_ = editPins(s, func(m map[string]int64) {
		cardID = m[draftID]
		delete(m, draftID)
	})
	if cardID == 0 {
		return
	}
	if err := pinBot(ctx, s, "unpinChatMessage", map[string]any{
		"chat_id": s.ApprovalChatID, "message_id": cardID,
	}, nil); err != nil {
		logEvent(s, "card_unpin_failed", "draft_id", draftID, "message_id", cardID, "error", err.Error())
	}
}

// sweepPins — карточки черновиков, решённых без карточки: протухшие —
// пометить и открепить; отправленные или отменённые мимо неё — открепить.
func sweepPins(ctx context.Context, s *config.Settings) {
	pins := map[string]int64{}
	_ = editPins(s, func(m map[string]int64) {
		for k, v := range m {
			pins[k] = v
		}
	})
	box := outbox.New(s.DataDir())
	now := time.Now()
	for id, cardID := range pins {
		d, err := box.Get(id)
		switch {
		case err != nil: // черновика нет — карточку держать незачем
		case d.Status == outbox.Pending && d.IsExpired(now), d.Status == outbox.Expired:
			bot.FinishCard(ctx, s, cardID, d, title(s, d), "⌛ **Протухло** — не нажато вовремя, сообщение не ушло.", d.Note)
		case isFinal(d.Status):
		default:
			continue // ещё ждёт владельца
		}
		unpinDraftCard(ctx, s, id)
	}
}

// maybeSweep — уборка из цикла автоотправки раз в минуту, в стороне:
// подвисший Bot API или отправка не должны задерживать автоотправку.
// Протухшие карточки — открепить; одобренное, которое никто не отправил, —
// дослать; запрос доступа, чья карточка не ушла, — отправить.
func maybeSweep(ctx context.Context, s *config.Settings, last *time.Time) {
	if time.Since(*last) < time.Minute || !sweeping.CompareAndSwap(false, true) {
		return
	}
	*last = time.Now()
	go func() {
		defer sweeping.Store(false)
		finishUnsaved(ctx, s) // ушедшее, чья отметка не легла в базу, — дописать
		// досылка — со сроком на каждый черновик и до остановки службы
		recoverApproved(ctx, s, time.Now().Add(-recoverOrphansAfter), "дослано: одобривший процесс не отправил")
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if s.BotReady() {
			retryCards(cctx, s) // итоги, которые Bot API не принял
			sweepPins(cctx, s)
			publishOrphanAccess(cctx, s)
		}
	}()
}
