package core

// Закреплённые карточки черновиков: карточка, которая ждёт нажатия владельца
// (ручное подтверждение), закреплена в чате с ботом, чтобы не потеряться среди
// карточек автоотправки, — без владельца такое сообщение не уйдёт. Решили
// (кнопкой, мимо кнопок, протухло) — откреплена. Какие закреплены —
// data/pinned_drafts.json (черновик → карточка).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"tgagent/internal/bot"
	"tgagent/internal/config"
	"tgagent/internal/outbox"
)

var (
	pinsMu   sync.Mutex
	pinBot   = bot.Call // тесты подменяют
	sweeping atomic.Bool
)

func pinsPath(s *config.Settings) string { return filepath.Join(s.DataDir(), "pinned_drafts.json") }

// editPins — прочитать, поменять f и записать (под pinsMu).
func editPins(s *config.Settings, f func(map[string]int64)) error {
	pinsMu.Lock()
	defer pinsMu.Unlock()
	path := pinsPath(s)
	pins := map[string]int64{}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if json.Unmarshal(raw, &pins) != nil {
			pins = map[string]int64{}
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	f(pins)
	raw, _ = json.MarshalIndent(pins, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
	box := outbox.New(s.OutboxPath())
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

// maybeSweepPins — из цикла автоотправки раз в минуту, в стороне: подвисший
// Bot API не должен задерживать автоотправку.
func maybeSweepPins(ctx context.Context, s *config.Settings, last *time.Time) {
	if time.Since(*last) < time.Minute || !s.BotReady() || !sweeping.CompareAndSwap(false, true) {
		return
	}
	*last = time.Now()
	go func() {
		defer sweeping.Store(false)
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		sweepPins(cctx, s)
	}()
}
