package core

// Слежение за обновлениями того, от чего зависит клиент MAX (internal/upstream):
// служба основного профиля раз в час проверяет веб-клиент MAX, каталог сборок
// Android-клиента и PyMax. Изменение — карточка владельцу в бот; она
// закреплена, пока он не нажмёт «Разобрано» (u:ack), — так не теряется среди
// карточек сообщений. Новая карточка источника заменяет его прежнюю, ещё не
// разобранную: та откреплена, без кнопки и с пометкой. Изменение, о котором
// бот не сообщил, не запоминается: следующая проверка пришлёт его снова.
// /updates — проверить сейчас.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tgagent/internal/bot"
	"tgagent/internal/config"
	"tgagent/internal/upstream"
)

const (
	upstreamEvery = time.Hour
	upstreamDelay = 2 * time.Minute // после старта службы: не мешать её разгону
	// столько проверок источника подряд с ошибкой — сказать владельцу (раз)
	upstreamFailNotice = 6
	upstreamAck        = "u:ack"
)

var (
	upstreamMu      sync.Mutex
	upstreamClient  = upstream.NewClient
	upstreamSources = upstream.Sources
	upstreamBot     = bot.Call
	upstreamAnswer  = bot.AnswerCallback
)

func upstreamPath(s *config.Settings) string { return filepath.Join(s.DataDir(), "upstream.json") }

// pinnedCard — закреплённая, ещё не разобранная карточка источника. Хранится
// отдельно от состояния проверок под своим коротким замком: «Разобрано» не
// ждёт проверку, которая держит upstreamMu, пока ходит в сеть.
type pinnedCard struct {
	ID   int64  `json:"id"`
	Text string `json:"text"` // HTML карточки: дописать пометку при замене
}

var pinnedMu sync.Mutex

func pinnedPath(s *config.Settings) string {
	return filepath.Join(s.DataDir(), "upstream_cards.json")
}

// updatePinned — прочитать карточки, поменять f и записать (под pinnedMu).
func updatePinned(s *config.Settings, f func(map[string]pinnedCard)) error {
	pinnedMu.Lock()
	defer pinnedMu.Unlock()
	path := pinnedPath(s)
	cards := map[string]pinnedCard{}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &cards); err != nil {
			cards = map[string]pinnedCard{} // испорчен — начать заново: пропадут лишь замены
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	f(cards)
	raw, err = json.MarshalIndent(cards, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RunUpstream — проверки по расписанию; одна на установку — в службе
// основного профиля.
func RunUpstream(ctx context.Context, loader func() (*config.Settings, error)) {
	if config.Profile() != "" {
		return
	}
	sleepCtx(ctx, upstreamDelay)
	for ctx.Err() == nil {
		if s, err := loader(); err == nil {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			_, _ = CheckUpstream(cctx, s)
			cancel()
		}
		sleepCtx(ctx, upstreamEvery)
	}
}

// CheckUpstream — проверить все источники, разослать карточки и сохранить
// состояние.
func CheckUpstream(ctx context.Context, s *config.Settings) (*upstream.State, error) {
	upstreamMu.Lock()
	defer upstreamMu.Unlock()
	path := upstreamPath(s)
	st, err := upstream.Load(path)
	if err != nil {
		logEvent(s, "upstream_check_failed", "error", err.Error())
		return nil, err
	}
	now := time.Now()
	first := false
	for _, r := range upstream.Check(ctx, upstreamClient(), st, upstreamSources()) {
		id := r.Source.ID
		switch {
		case r.Err != nil:
			if ctx.Err() != nil {
				continue // служба останавливается — это не сбой источника
			}
			st.Apply(r, now)
			logEvent(s, "upstream_check_failed", "source", id, "error", r.Err.Error(), "fails", st.Sources[id].Fails)
			if st.Sources[id].Fails == upstreamFailNotice && s.BotReady() {
				text := fmt.Sprintf("⚠️ <b>Не удаётся проверить: %s</b>\n\n%d проверок подряд с ошибкой:\n<code>%s</code>\n\n"+
					"Если источник переехал или поменял формат — слежение за ним нужно поправить.",
					html.EscapeString(r.Source.Title), upstreamFailNotice, html.EscapeString(r.Err.Error()))
				_ = sendPinned(ctx, s, text, id)
			}
		case r.Changed():
			if s.BotReady() {
				if err := sendPinned(ctx, s, upstreamCard(r), id); err != nil {
					continue // не запоминаем: сообщим со следующей проверкой
				}
			}
			logEvent(s, "upstream_changed", "source", id, "changes", diffsText(r.Diffs))
			st.Apply(r, now)
		default:
			first = first || r.First
			st.Apply(r, now)
		}
	}
	if err := st.Save(path); err != nil {
		logEvent(s, "upstream_check_failed", "error", err.Error())
		return st, err
	}
	if first && s.BotReady() {
		upstreamSay(ctx, s, upstreamSummary(st, "🔭 <b>Слежение за обновлениями MAX включено</b>"))
	}
	return st, nil
}

func diffsText(diffs []upstream.Diff) string {
	parts := make([]string, len(diffs))
	for i, d := range diffs {
		parts[i] = fmt.Sprintf("%s: %s → %s", d.Label, d.Old, d.New)
	}
	return strings.Join(parts, "; ")
}

// upstreamCard — карточка изменения (HTML).
func upstreamCard(r upstream.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔔 <b>Обновление: %s</b>\n\n", html.EscapeString(r.Source.Title))
	for _, d := range r.Diffs {
		fmt.Fprintf(&b, "%s: <code>%s</code> → <code>%s</code>\n",
			html.EscapeString(d.Label), html.EscapeString(d.Old), html.EscapeString(d.New))
	}
	if len(r.Details) > 0 {
		b.WriteString("\n")
		// лимит сообщения — 4096; режем по строкам, чтобы не разорвать тег
		for _, d := range r.Details {
			line := html.EscapeString(truncRunes(d, 700)) + "\n"
			if len([]rune(b.String()))+len([]rune(line)) > 3500 {
				b.WriteString("…\n")
				break
			}
			b.WriteString(line)
		}
	}
	link := r.Cur["url"]
	if link == "" {
		link = r.Source.URL
	}
	fmt.Fprintf(&b, "\n<a href=\"%s\">Открыть</a>", html.EscapeString(link))
	return b.String()
}

// upstreamSay — сообщение владельцу без кнопок (через upstreamBot: тесты).
func upstreamSay(ctx context.Context, s *config.Settings, text string) {
	if err := upstreamBot(ctx, s, "sendMessage", map[string]any{
		"chat_id": s.ApprovalChatID, "text": text, "parse_mode": "HTML",
		"link_preview_options": map[string]any{"is_disabled": true},
	}, nil); err != nil {
		logEvent(s, "notify_failed", "what", "upstream", "error", err.Error())
	}
}

// upstreamSummary — что видно у источников сейчас (для /updates).
func upstreamSummary(st *upstream.State, title string) string {
	var b strings.Builder
	b.WriteString(title + "\n\n")
	for _, src := range upstreamSources() {
		ss := st.Sources[src.ID]
		fmt.Fprintf(&b, "<b>%s</b>", html.EscapeString(src.Title))
		switch {
		case ss == nil:
			b.WriteString(": ещё не проверялся\n")
			continue
		case ss.Error != "":
			fmt.Fprintf(&b, ": ⚠️ ошибка (%d подряд) — <code>%s</code>\n", ss.Fails, html.EscapeString(truncRunes(ss.Error, 200)))
			continue
		}
		b.WriteString("\n")
		for _, l := range src.Labels {
			fmt.Fprintf(&b, "  %s: <code>%s</code>\n", html.EscapeString(l.Text), html.EscapeString(ss.Values[l.Key]))
		}
	}
	b.WriteString("\nПроверка раз в час. Изменение придёт отдельной закреплённой карточкой; «Разобрано» её открепит.")
	return b.String()
}

// sendPinned — карточка с кнопкой «Разобрано», закреплённая в чате с ботом.
func sendPinned(ctx context.Context, s *config.Settings, text, source string) error {
	var sent struct {
		MessageID int64 `json:"message_id"`
	}
	kb := map[string]any{"inline_keyboard": [][]map[string]string{{
		{"text": "✅ Разобрано", "callback_data": s.CallbackPrefix() + upstreamAck},
	}}}
	if err := upstreamBot(ctx, s, "sendMessage", map[string]any{
		"chat_id": s.ApprovalChatID, "text": text, "parse_mode": "HTML",
		"link_preview_options": map[string]any{"is_disabled": true}, "reply_markup": kb,
	}, &sent); err != nil {
		logEvent(s, "notify_failed", "what", "upstream", "source", source, "error", err.Error())
		return err
	}
	// уведомление уже было от самого сообщения — второе («закрепил») ни к чему
	if err := upstreamBot(ctx, s, "pinChatMessage", map[string]any{
		"chat_id": s.ApprovalChatID, "message_id": sent.MessageID, "disable_notification": true,
	}, nil); err != nil {
		logEvent(s, "upstream_pin_failed", "source", source, "message_id", sent.MessageID, "error", err.Error())
	}
	var old pinnedCard
	if err := updatePinned(s, func(m map[string]pinnedCard) {
		old = m[source]
		m[source] = pinnedCard{ID: sent.MessageID, Text: text}
	}); err != nil {
		logEvent(s, "upstream_pin_failed", "source", source, "message_id", sent.MessageID, "error", err.Error())
	}
	if old.ID != 0 {
		retirePinned(ctx, s, old)
	}
	return nil
}

// retirePinned — прежняя карточка источника, которую заменила новая:
// открепить (новая уже закреплена — плашка наверху показывает её), убрать
// кнопку и пометить.
func retirePinned(ctx context.Context, s *config.Settings, c pinnedCard) {
	_ = upstreamBot(ctx, s, "unpinChatMessage", map[string]any{"chat_id": s.ApprovalChatID, "message_id": c.ID}, nil)
	err := errors.New("текст карточки неизвестен")
	if c.Text != "" {
		err = upstreamBot(ctx, s, "editMessageText", map[string]any{
			"chat_id": s.ApprovalChatID, "message_id": c.ID, "parse_mode": "HTML",
			"text":                 c.Text + "\n\n<i>Заменена новой карточкой ↓</i>",
			"link_preview_options": map[string]any{"is_disabled": true},
		}, nil)
	}
	if err != nil { // пометить не вышло — хотя бы снять кнопку
		_ = upstreamBot(ctx, s, "editMessageReplyMarkup", map[string]any{
			"chat_id": s.ApprovalChatID, "message_id": c.ID, "reply_markup": map[string]any{"inline_keyboard": [][]any{}},
		}, nil)
	}
	logEvent(s, "upstream_card_replaced", "message_id", c.ID)
}

// handleUpstreamCallback — «Разобрано»: открепить карточку и убрать кнопку.
func handleUpstreamCallback(ctx context.Context, s *config.Settings, q *bot.CallbackQuery) {
	if q.Message == nil {
		upstreamAnswer(ctx, s, q.ID, "Не вижу, какое сообщение открепить.")
		return
	}
	id := q.Message.MessageID
	if err := upstreamBot(ctx, s, "unpinChatMessage", map[string]any{
		"chat_id": s.ApprovalChatID, "message_id": id,
	}, nil); err != nil {
		upstreamAnswer(ctx, s, q.ID, "Не открепилось: "+err.Error())
		logEvent(s, "upstream_pin_failed", "message_id", id, "error", err.Error())
		return
	}
	_ = upstreamBot(ctx, s, "editMessageReplyMarkup", map[string]any{
		"chat_id": s.ApprovalChatID, "message_id": id, "reply_markup": map[string]any{"inline_keyboard": [][]any{}},
	}, nil)
	_ = updatePinned(s, func(m map[string]pinnedCard) {
		for src, c := range m {
			if c.ID == id {
				delete(m, src)
			}
		}
	})
	logEvent(s, "upstream_acked", "message_id", id)
	upstreamAnswer(ctx, s, q.ID, "Откреплено.")
}

// upstreamCommand — /updates: проверить сейчас и показать, что видно.
func upstreamCommand(ctx context.Context, s *config.Settings) {
	if config.Profile() != "" {
		return // слежение — у службы основного профиля
	}
	go func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		st, err := CheckUpstream(cctx, s)
		if err != nil {
			upstreamSay(cctx, s, "Проверка не удалась: "+html.EscapeString(err.Error()))
			return
		}
		upstreamSay(cctx, s, upstreamSummary(st, "🔭 <b>Обновления MAX и PyMax</b> — проверено сейчас"))
	}()
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
