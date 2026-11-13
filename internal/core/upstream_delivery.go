package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/lock"
	"tgagent/internal/store"
	"tgagent/internal/upstream"
)

// A notice is saved before either external side effect. Keep the last resolved
// generation too: a lost source-state write must not reopen an acknowledged job.
const upstreamNoticesKey = "upstream_notices"

type upstreamNotice struct {
	Event       string       `json:"event"`
	Title       string       `json:"title"`
	Brief       string       `json:"brief"`
	Text        string       `json:"text"`
	At          time.Time    `json:"at"`
	TaskPending bool         `json:"task_pending,omitempty"`
	Agent       bool         `json:"agent,omitempty"`
	CardID      int64        `json:"card_id,omitempty"`
	CardPending bool         `json:"card_pending,omitempty"`
	Pinned      bool         `json:"pinned,omitempty"`
	Done        bool         `json:"done,omitempty"`
	Escalated   bool         `json:"escalated,omitempty"`
	Note        string       `json:"note,omitempty"`
	Previous    []pinnedCard `json:"previous,omitempty"`
	Projection  *pinnedCard  `json:"projection,omitempty"` // last row written to the legacy-compatible table
}

func upstreamNotices(s *config.Settings) (map[string]upstreamNotice, error) {
	var m map[string]upstreamNotice
	err := editUpstreamNotices(s, func(current map[string]upstreamNotice, _ *sql.Tx) error { m = current; return nil })
	return m, err
}

func editUpstreamNotices(s *config.Settings, f func(map[string]upstreamNotice, *sql.Tx) error) error {
	db, err := stateDB(s)
	if err != nil {
		return err
	}
	ctx := context.Background()
	return db.Write(ctx, func(tx *sql.Tx) error {
		var m map[string]upstreamNotice
		if _, err := store.GetKV(ctx, tx, upstreamNoticesKey, &m); err != nil {
			return err
		}
		if m == nil {
			m = map[string]upstreamNotice{}
		}
		// Older SQLite builds and rollback JSON imports only update cardsTab.
		// A changed projection is evidence of that writer, not stale new state
		// which may override its acknowledgement, escalation or replacement.
		for source, n := range m {
			card, exists, err := cardsTab.Get(ctx, tx, source)
			if err != nil {
				return err
			}
			if n.Projection == nil && !exists || n.Projection != nil && exists && sameUpstreamCard(*n.Projection, card) {
				continue
			}
			if !exists {
				n.Done, n.TaskPending, n.CardPending, n.Pinned = true, false, false, false
				n.Note, n.Projection, n.Previous = "отмечено прежней сборкой", nil, nil
				m[source] = n
			} else {
				delete(m, source) // the older writer's live row owns this source now
			}
		}
		if err := f(m, tx); err != nil {
			return err
		}
		return store.PutKV(ctx, tx, upstreamNoticesKey, m)
	})
}

func sameUpstreamCard(a, b pinnedCard) bool {
	return a.ID == b.ID && a.Text == b.Text && a.Agent == b.Agent && a.At.Equal(b.At)
}

func editUpstreamNotice(s *config.Settings, source, event string, f func(*upstreamNotice, *sql.Tx) error) error {
	return editUpstreamNotices(s, func(m map[string]upstreamNotice, tx *sql.Tx) error {
		n, ok := m[source]
		if !ok || n.Event != event {
			return nil
		}
		if err := f(&n, tx); err != nil {
			return err
		}
		m[source] = n
		return nil
	})
}

func queueUpstreamNotice(ctx context.Context, s *config.Settings, text, source, title, brief, occurrence string) error {
	event := fmt.Sprintf("%x", sha256.Sum256([]byte(source+"\x00"+text+"\x00"+title+"\x00"+brief+"\x00"+occurrence)))
	if err := editUpstreamNotices(s, func(m map[string]upstreamNotice, tx *sql.Tx) error {
		if m[source].Event == event {
			return nil
		}
		previous := m[source].Previous
		if old := m[source]; old.CardID != 0 && (!old.Done || old.CardPending || old.Pinned) {
			previous = append(previous, pinnedCard{ID: old.CardID, Text: old.Text})
		} else if old, ok, err := cardsTab.Get(context.Background(), tx, source); err != nil {
			return err
		} else if ok && old.ID != 0 {
			previous = append(previous, old)
		}
		legacy, exists, err := cardsTab.Get(context.Background(), tx, source)
		if err != nil {
			return err
		}
		var projection *pinnedCard
		if exists {
			projection = &legacy
		}
		m[source] = upstreamNotice{Event: event, Text: text, Title: title, Brief: brief, Projection: projection,
			At: time.Now(), TaskPending: true, CardPending: true, Previous: previous}
		return nil
	}); err != nil {
		return err
	}
	// The durable job is accepted even if a transport is unavailable. The service
	// retries it independently of whether the source changes again.
	if err := deliverUpstreamNotice(ctx, s, source); err != nil {
		logEvent(s, "upstream_delivery_pending", "source", source, "error", err.Error())
	}
	return nil
}

func upstreamTask(source string, n upstreamNotice) string {
	check := "Проверь, затрагивает ли это клиент MAX и слежение за обновлениями."
	for _, src := range upstream.Sources() {
		if src.ID == source && src.Task != "" {
			check = src.Task
		}
	}
	return n.Brief + "\nСобытие зарегистрировано; карточка владельцу доставляется отдельно. " +
		check + " Нужна правка — сделай её сам по правилам проекта (AGENTS.md). Потом: " +
		"внимания владельца не нужно — bin\\tg.exe updates ack " + source + " \"<вывод>\"; " +
		"нужно его решение — bin\\tg.exe updates escalate " + source + " \"<что решить>\"."
}

func deliverUpstreamNotice(ctx context.Context, s *config.Settings, source string) error {
	name := fmt.Sprintf("%x", sha256.Sum256([]byte(source)))
	dir := filepath.Join(s.DataDir(), "upstream-delivery")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return lock.With(ctx, filepath.Join(dir, name), time.Second, func() error {
		m, err := upstreamNotices(s)
		if err != nil {
			return err
		}
		n, ok := m[source]
		if !ok {
			return nil
		}
		if n.TaskPending && !n.Done && !n.Escalated && upstreamWake != nil {
			wakeErr := upstreamWake(ctx, s, n.Title, upstreamTask(source, n))
			if wakeErr != nil {
				logEvent(s, "upstream_wake_failed", "source", source, "error", wakeErr.Error())
			} else {
				if err := editUpstreamNotice(s, source, n.Event, func(cur *upstreamNotice, _ *sql.Tx) error {
					if !cur.Done && !cur.Escalated {
						cur.Agent, cur.TaskPending, cur.CardPending = true, false, true
						cur.At = time.Now()
					}
					return nil
				}); err != nil {
					return err
				}
				logEvent(s, "upstream_agent_woken", "source", source, "event", n.Event)
			}
		}
		return showUpstreamNotice(ctx, s, source, n.Event)
	})
}

func noticeText(n upstreamNotice) string {
	switch {
	case n.Done:
		return n.Text + "\n\n<i>✅ Разобрано: " + html.EscapeString(truncRunes(n.Note, 500)) + "</i>"
	case n.Escalated:
		return n.Text + "\n\n<b>⚠️ Нужно твоё решение:</b> " + html.EscapeString(truncRunes(n.Note, 800))
	case n.Agent:
		return n.Text + upstreamAgentMark
	default:
		return n.Text
	}
}

func showUpstreamNotice(ctx context.Context, s *config.Settings, source, event string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	m, err := upstreamNotices(s)
	if err != nil {
		return err
	}
	n, ok := m[source]
	if !ok || n.Event != event || !n.CardPending {
		return nil
	}
	if n.Done && n.CardID == 0 {
		for _, old := range n.Previous {
			if err := retirePinned(ctx, s, old); err != nil {
				return err
			}
		}
		return editUpstreamNotice(s, source, event, func(cur *upstreamNotice, _ *sql.Tx) error {
			cur.CardPending, cur.Previous = false, nil
			return nil
		})
	}
	if !s.BotReady() {
		return nil
	}
	shown := noticeText(n)
	if n.CardID == 0 {
		var sent struct {
			MessageID int64 `json:"message_id"`
		}
		if err := upstreamBot(ctx, s, "sendMessage", map[string]any{
			"chat_id": s.ApprovalChatID, "text": shown, "parse_mode": "HTML",
			"disable_notification": n.Agent, "reply_markup": upstreamKeyboard(s),
			"link_preview_options": map[string]any{"is_disabled": true},
		}, &sent); err != nil {
			logEvent(s, "notify_failed", "what", "upstream", "source", source, "error", err.Error())
			return err
		}
		if sent.MessageID == 0 {
			return fmt.Errorf("upstream card has no message ID")
		}
		if err := editUpstreamNotice(s, source, event, func(cur *upstreamNotice, _ *sql.Tx) error {
			cur.CardID = sent.MessageID
			return nil
		}); err != nil {
			return err
		}
		m, err = upstreamNotices(s)
		if err != nil {
			return err
		}
		if m[source].Event != event {
			retirePinned(ctx, s, pinnedCard{ID: sent.MessageID, Text: n.Text})
			return nil
		}
		n = m[source] // ack/escalate may have happened while sendMessage waited
		if shown == noticeText(n) {
			shown = "" // already displayed; avoid an unnecessary edit
		}
	}
	if shown != "" {
		markup := any(upstreamKeyboard(s))
		if n.Done {
			markup = map[string]any{"inline_keyboard": [][]any{}}
		}
		if err := upstreamBot(ctx, s, "editMessageText", map[string]any{
			"chat_id": s.ApprovalChatID, "message_id": n.CardID, "text": noticeText(n), "parse_mode": "HTML",
			"reply_markup": markup, "link_preview_options": map[string]any{"is_disabled": true},
		}, nil); err != nil && !strings.Contains(err.Error(), "message is not modified") {
			return err
		}
	}
	pinned := !n.Agent && !n.Done
	if pinned || n.Pinned {
		method := "pinChatMessage"
		if pinned {
			// A lost pin receipt or process death must still allow a later
			// resolution to unpin this known card.
			if err := editUpstreamNotice(s, source, event, func(cur *upstreamNotice, _ *sql.Tx) error {
				cur.Pinned = true
				return nil
			}); err != nil {
				return err
			}
		}
		if !pinned {
			method = "unpinChatMessage"
		}
		if err := upstreamBot(ctx, s, method, map[string]any{
			"chat_id": s.ApprovalChatID, "message_id": n.CardID, "disable_notification": false,
		}, nil); err != nil {
			return err
		}
	}
	for _, old := range n.Previous {
		if err := retirePinned(ctx, s, old); err != nil {
			return err
		}
	}
	return editUpstreamNotice(s, source, event, func(cur *upstreamNotice, tx *sql.Tx) error {
		cur.Pinned = pinned
		// A concurrent resolution must retain its pending presentation.
		if cur.Done != n.Done || cur.Escalated != n.Escalated || cur.Note != n.Note || cur.Agent != n.Agent {
			return nil
		}
		cur.CardPending, cur.Pinned, cur.Previous = false, pinned, nil
		if cur.Done {
			cur.Projection = nil
			return cardsTab.Delete(context.Background(), tx, source)
		}
		card := pinnedCard{ID: cur.CardID, Text: cur.Text, Agent: cur.Agent, At: cur.At}
		cur.Projection = &card
		return cardsTab.Put(context.Background(), tx, source, card)
	})
}

func retryUpstreamNotices(ctx context.Context, s *config.Settings) {
	m, err := upstreamNotices(s)
	if err != nil {
		logEvent(s, "upstream_delivery_pending", "error", err.Error())
		return
	}
	for source, n := range m {
		if n.TaskPending || n.CardPending {
			if err := deliverUpstreamNotice(ctx, s, source); err != nil {
				logEvent(s, "upstream_delivery_pending", "source", source, "error", err.Error())
			}
		}
	}
}

// Resolution is durable before presentation. In particular an agent can ack
// while the initial card has no ID yet; its late reply cannot reopen the task.
func resolveUpstreamNotice(ctx context.Context, s *config.Settings, source, event, note string, done bool) error {
	applied := false
	if err := editUpstreamNotice(s, source, event, func(n *upstreamNotice, tx *sql.Tx) error {
		if n.Done && !done {
			return fmt.Errorf("обновление уже разобрано")
		}
		applied = true
		n.Done, n.Escalated, n.Note = done, !done, note
		n.TaskPending, n.CardPending = false, true
		if done {
			n.Projection = nil
			return cardsTab.Delete(context.Background(), tx, source)
		}
		n.Agent = false
		return nil
	}); err != nil {
		return err
	}
	if !applied {
		return fmt.Errorf("обновление источника изменилось; перечитай tg updates")
	}
	m, err := upstreamNotices(s)
	if err != nil {
		return err
	}
	if n := m[source]; n.Event == event && n.CardID != 0 {
		// Another publisher may own the lock. It will re-read the resolution;
		// failure here does not undo an already recorded decision.
		if err := deliverUpstreamNotice(ctx, s, source); err != nil {
			logEvent(s, "upstream_delivery_pending", "source", source, "error", err.Error())
		}
	}
	return nil
}
