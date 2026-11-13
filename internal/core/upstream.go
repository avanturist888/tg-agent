package core

// Слежение за обновлениями того, от чего зависят клиенты MAX, Telegram и WhatsApp
// (internal/upstream): служба основного профиля раз в час проверяет веб-клиент
// MAX, каталог сборок Android-клиента, PyMax, релизы gotd и ветку whatsmeow;
// служба WhatsApp сама зовёт разбор, если сервер отверг её клиент как
// устаревший (core/wa.go). Изменение сначала разбирает агент, а не владелец
// (он не передатчик): задание записывается до обращения к транспорту,
// передаётся агенту независимо от бота; карточка доставляется отдельно.
// Агент проекта просыпается только в назначенном треде (tg updates thread)
// (через Нестора) — проверяет, чинит сам и
// снимает карточку (tg updates ack) или зовёт владельца (tg updates escalate:
// закрепить со звуком). Агента не разбудить — карточка сразу владельцу,
// закреплённая, пока он не нажмёт «Разобрано» (u:ack). Новая карточка
// источника заменяет его прежнюю, ещё не разобранную: та откреплена, без
// кнопки и с пометкой. Не доведённые задания и карточки повторяются из
// сохранённого состояния независимо от новых изменений. /updates — проверить сейчас.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tgagent/internal/bot"
	"tgagent/internal/config"
	"tgagent/internal/store"
	"tgagent/internal/upstream"
)

const (
	upstreamEvery = time.Hour
	upstreamDelay = 2 * time.Minute // после старта службы: не мешать её разгону
	// столько проверок источника подряд с ошибкой — сказать владельцу (раз)
	upstreamFailNotice = 6
	upstreamAck        = "u:ack"
	upstreamAgentWait  = 6 * time.Hour
)

var (
	upstreamMu      sync.Mutex
	upstreamClient  = upstream.NewClient
	upstreamSources = upstream.Sources
	upstreamBot     = bot.Call
	upstreamAnswer  = bot.AnswerCallback
	upstreamWake    = wakeProjectAgent
)

// pinnedCard — закреплённая, ещё не разобранная карточка источника. Хранится
// отдельно от состояния проверок под своим коротким замком: «Разобрано» не
// ждёт проверку, которая держит upstreamMu, пока ходит в сеть.
type pinnedCard struct {
	ID    int64  `json:"id"`
	Text  string `json:"text"`            // HTML карточки: дописать пометку при замене
	Agent bool   `json:"agent,omitempty"` // разбирает агент: не закреплена
	// At — когда карточку отдали агенту: не разобрал за upstreamAgentWait —
	// она уходит владельцу (письмо в занятый тред могло так и не дойти)
	At time.Time `json:"at,omitempty"`
}

var cardsTab = store.NewTable[pinnedCard](store.TUpstreamCards)

// updatePinned — прочитать карточки, поменять f и записать одной транзакцией.
func updatePinned(s *config.Settings, f func(map[string]pinnedCard)) error {
	db, err := stateDB(s)
	if err != nil {
		return err
	}
	return cardsTab.Edit(context.Background(), db, func(m map[string]pinnedCard) bool { f(m); return true })
}

// RunUpstream — проверки по расписанию; одна на установку — в службе
// основного профиля.
func RunUpstream(ctx context.Context, loader func() (*config.Settings, error)) {
	if config.Profile() != "" {
		return
	}
	sleepCtx(ctx, upstreamDelay)
	var nextCheck time.Time
	for ctx.Err() == nil {
		if s, err := loader(); err == nil {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			retryUpstreamNotices(cctx, s)
			if !time.Now().Before(nextCheck) {
				_, _ = CheckUpstream(cctx, s)
				nextCheck = time.Now().Add(upstreamEvery)
			}
			cancel()
		}
		sleepCtx(ctx, time.Minute)
	}
}

// CheckUpstream — проверить все источники, разослать карточки и сохранить
// состояние.
func CheckUpstream(ctx context.Context, s *config.Settings) (*upstream.State, error) {
	upstreamMu.Lock()
	defer upstreamMu.Unlock()
	st, err := upstream.Load(s.DataDir())
	if err != nil {
		logEvent(s, "upstream_check_failed", "error", err.Error())
		return nil, err
	}
	now := time.Now()
	escalateStale(ctx, s, now)
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
			if st.Sources[id].Fails == upstreamFailNotice && (s.BotReady() || upstreamWake != nil) {
				text := fmt.Sprintf("⚠️ <b>Не удаётся проверить: %s</b>\n\n%d проверок подряд с ошибкой:\n<code>%s</code>\n\n"+
					"Если источник переехал или поменял формат — слежение за ним нужно поправить.",
					html.EscapeString(r.Source.Title), upstreamFailNotice, html.EscapeString(r.Err.Error()))
				brief := fmt.Sprintf("Слежение за обновлениями не может проверить источник «%s» (%s): "+
					"%d проверок подряд с ошибкой: %s\nИсточник мог переехать или поменять формат — "+
					"почини проверку в internal/upstream.", r.Source.Title, id, upstreamFailNotice, r.Err.Error())
				_ = sendPinned(ctx, s, text, id, "Не удаётся проверить: "+r.Source.Title, brief)
			}
		case r.Changed():
			if s.BotReady() || upstreamWake != nil {
				if err := sendPinned(ctx, s, upstreamCard(r), id, "Обновление: "+r.Source.Title, upstreamBrief(r)); err != nil {
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
	if err := st.Save(s.DataDir()); err != nil {
		logEvent(s, "upstream_check_failed", "error", err.Error())
		return st, err
	}
	if first && s.BotReady() {
		upstreamSay(ctx, s, upstreamSummary(st, "🔭 <b>Слежение за обновлениями включено</b>"))
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

const upstreamAgentMark = "\n\n<i>📨 Задание передано агенту tg-agent — позовёт, если нужно твоё решение.</i>"

func upstreamKeyboard(s *config.Settings) map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{{
		{"text": "✅ Разобрано", "callback_data": s.CallbackPrefix() + upstreamAck},
	}}}
}

// upstreamBrief — изменение для агента, простым текстом.
func upstreamBrief(r upstream.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Слежение за обновлениями: изменение у источника «%s» (%s).\n", r.Source.Title, r.Source.ID)
	for _, d := range r.Diffs {
		fmt.Fprintf(&b, "%s: %s → %s\n", d.Label, d.Old, d.New)
	}
	for _, d := range r.Details {
		b.WriteString(truncRunes(d, 700) + "\n")
	}
	link := r.Cur["url"]
	if link == "" {
		link = r.Source.URL
	}
	b.WriteString("Ссылка: " + link + "\n")
	return b.String()
}

// escalateStale — карточки, которые агент не разобрал за upstreamAgentWait,
// — владельцу: агент мог не получить задание (письмо ждало в очереди треда,
// а тред закрыли) или не довести его до ack/escalate. Не вышло — повторим
// со следующей проверкой.
func escalateStale(ctx context.Context, s *config.Settings, now time.Time) {
	var stale []string
	notices, err := upstreamNotices(s)
	if err != nil {
		logEvent(s, "upstream_stale_failed", "error", err.Error())
		return
	}
	for source, n := range notices {
		if n.Agent && !n.Done && !n.Escalated && now.Sub(n.At) >= upstreamAgentWait {
			stale = append(stale, source)
		}
	}
	_ = updatePinned(s, func(m map[string]pinnedCard) {
		for src, c := range m {
			if _, ok := notices[src]; ok {
				continue
			}
			switch {
			case !c.Agent:
			case c.At.IsZero(): // карточка из прежней сборки — отсчёт с сейчас
				c.At = now
				m[src] = c
			case now.Sub(c.At) >= upstreamAgentWait:
				stale = append(stale, src)
			}
		}
	})
	for _, src := range stale {
		note := fmt.Sprintf("агент не разобрал за %d ч (мог не получить задание) — посмотри сам или напиши агенту tg-agent.",
			int(upstreamAgentWait.Hours()))
		if _, err := EscalateUpstream(ctx, s, src, note); err != nil {
			logEvent(s, "upstream_stale_failed", "source", src, "error", err.Error())
			continue
		}
		logEvent(s, "upstream_agent_stale", "source", src)
	}
}

// sendPinned — карточка изменения с кнопкой «Разобрано». Сначала её
// разбирает агент: карточка без звука и не закреплена. Агента не разбудить —
// закреплена для владельца.
func sendPinned(ctx context.Context, s *config.Settings, text, source, title, brief string) error {
	if upstreamWake != nil && brief != "" {
		return queueUpstreamNotice(ctx, s, text, source, title, brief)
	}
	// No agent route requested: retain the direct owner notification path.
	var sent struct {
		MessageID int64 `json:"message_id"`
	}
	if err := upstreamBot(ctx, s, "sendMessage", map[string]any{
		"chat_id": s.ApprovalChatID, "text": text, "parse_mode": "HTML", "disable_notification": false,
		"link_preview_options": map[string]any{"is_disabled": true}, "reply_markup": upstreamKeyboard(s),
	}, &sent); err != nil {
		logEvent(s, "notify_failed", "what", "upstream", "source", source, "error", err.Error())
		return err
	}
	if err := upstreamBot(ctx, s, "pinChatMessage", map[string]any{
		"chat_id": s.ApprovalChatID, "message_id": sent.MessageID, "disable_notification": true,
	}, nil); err != nil {
		logEvent(s, "upstream_pin_failed", "source", source, "message_id", sent.MessageID, "error", err.Error())
	}
	var old pinnedCard
	if err := editUpstreamNotices(s, func(m map[string]upstreamNotice, tx *sql.Tx) error {
		var err error
		old, _, err = cardsTab.Get(context.Background(), tx, source)
		if err != nil {
			return err
		}
		delete(m, source) // an explicit owner-only incident supersedes the agent job
		return cardsTab.Put(context.Background(), tx, source, pinnedCard{ID: sent.MessageID, Text: text, At: time.Now()})
	}); err != nil {
		return err
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
//
// Решение («разобрано») — запись в базе: она снимается первой, и сбой базы
// — retryable (нажатие разберётся снова), а не «Откреплено» при живой
// записи, которая потом снова дошла бы до владельца эскалацией. Сбой
// открепления — тоже повтор: запись уже снята, повтор только открепит.
func handleUpstreamCallback(ctx context.Context, s *config.Settings, q *bot.CallbackQuery) error {
	if q.Message == nil {
		upstreamAnswer(ctx, s, q.ID, "Не вижу, какое сообщение открепить.")
		return nil
	}
	id := q.Message.MessageID
	agent := false // карточку разбирал агент — она и не закреплена
	if err := editUpstreamNotices(s, func(m map[string]upstreamNotice, tx *sql.Tx) error {
		for source, n := range m {
			if n.CardID != 0 && n.CardID == id {
				agent = agent || n.Agent
				n.Done, n.TaskPending, n.CardPending, n.Note = true, false, true, "отмечено владельцем"
				m[source] = n
				if err := cardsTab.Delete(context.Background(), tx, source); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return retryable{err}
	}
	if err := updatePinned(s, func(m map[string]pinnedCard) {
		for src, c := range m {
			if c.ID == id {
				agent = agent || c.Agent
				delete(m, src)
			}
		}
	}); err != nil {
		return retryable{err}
	}
	if err := upstreamBot(ctx, s, "unpinChatMessage", map[string]any{
		"chat_id": s.ApprovalChatID, "message_id": id,
	}, nil); err != nil && !agent {
		logEvent(s, "upstream_pin_failed", "message_id", id, "error", err.Error())
		return retryable{err}
	}
	_ = upstreamBot(ctx, s, "editMessageReplyMarkup", map[string]any{
		"chat_id": s.ApprovalChatID, "message_id": id, "reply_markup": map[string]any{"inline_keyboard": [][]any{}},
	}, nil)
	logEvent(s, "upstream_acked", "message_id", id)
	upstreamAnswer(ctx, s, q.ID, "Откреплено.")
	return nil
}

// UpstreamCard — закреплённая, ещё не разобранная карточка (tg updates).
type UpstreamCard struct {
	Source      string `json:"source"`
	Title       string `json:"title"`
	MessageID   int64  `json:"message_id"`
	Agent       bool   `json:"agent,omitempty"` // разбирает агент, не закреплена
	CardPending bool   `json:"card_pending,omitempty"`
	Event       string `json:"-"` // generation captured for concurrent ack/escalate
}

// PinnedUpstream — карточки, которые ждут разбора.
func PinnedUpstream(s *config.Settings) ([]UpstreamCard, error) {
	if config.Profile() != "" {
		return nil, &Bad{Msg: "карточки обновлений — у основного профиля: без --profile"}
	}
	notices, err := upstreamNotices(s)
	if err != nil {
		return nil, err
	}
	var out []UpstreamCard
	err = updatePinned(s, func(m map[string]pinnedCard) {
		for _, src := range upstreamSources() {
			if n, ok := notices[src.ID]; ok {
				if !n.Done {
					out = append(out, UpstreamCard{Source: src.ID, Title: src.Title, MessageID: n.CardID, Agent: n.Agent, CardPending: n.CardPending, Event: n.Event})
				}
				continue
			}
			if c, ok := m[src.ID]; ok {
				out = append(out, UpstreamCard{Source: src.ID, Title: src.Title, MessageID: c.ID, Agent: c.Agent})
			}
		}
	})
	return out, err
}

// AckUpstream — «Разобрано» за владельца: агент проверил изменение, и
// внимания оно не требует. Карточка откреплена, без кнопки, с выводом агента
// (note). source — id источника или "all".
func AckUpstream(ctx context.Context, s *config.Settings, source, note string) ([]UpstreamCard, error) {
	note = strings.TrimSpace(note)
	if note == "" {
		return nil, &Bad{Msg: "нужен вывод: почему внимание владельца не нужно"}
	}
	cards, err := PinnedUpstream(s)
	if err != nil {
		return nil, err
	}
	var done []UpstreamCard
	for _, c := range cards {
		if source != "all" && c.Source != source {
			continue
		}
		if c.Event != "" {
			if err := resolveUpstreamNotice(ctx, s, c.Source, c.Event, note, true); err != nil {
				return done, err
			}
			logEvent(s, "upstream_acked", "source", c.Source, "message_id", c.MessageID, "by", "agent")
			done = append(done, c)
			continue
		}
		var pc pinnedCard
		_ = updatePinned(s, func(m map[string]pinnedCard) { pc = m[c.Source] })
		if pc.Agent {
			// не закреплена — снимать нечего
		} else if err := upstreamBot(ctx, s, "unpinChatMessage", map[string]any{
			"chat_id": s.ApprovalChatID, "message_id": c.MessageID,
		}, nil); err != nil {
			logEvent(s, "upstream_pin_failed", "message_id", c.MessageID, "error", err.Error())
			return done, fmt.Errorf("%s: не открепилось: %w", c.Source, err)
		}
		err := errors.New("текст карточки неизвестен")
		if pc.Text != "" { // без reply_markup правка снимает и кнопку
			err = upstreamBot(ctx, s, "editMessageText", map[string]any{
				"chat_id": s.ApprovalChatID, "message_id": c.MessageID, "parse_mode": "HTML",
				"text":                 pc.Text + "\n\n<i>✅ Разобрал агент: " + html.EscapeString(truncRunes(note, 500)) + "</i>",
				"link_preview_options": map[string]any{"is_disabled": true},
			}, nil)
		}
		if err != nil {
			_ = upstreamBot(ctx, s, "editMessageReplyMarkup", map[string]any{
				"chat_id": s.ApprovalChatID, "message_id": c.MessageID, "reply_markup": map[string]any{"inline_keyboard": [][]any{}},
			}, nil)
		}
		_ = updatePinned(s, func(m map[string]pinnedCard) {
			if m[c.Source].ID == c.MessageID {
				delete(m, c.Source)
			}
		})
		logEvent(s, "upstream_acked", "message_id", c.MessageID, "source", c.Source, "by", "agent")
		done = append(done, c)
	}
	if len(done) == 0 && source != "all" {
		return nil, &Bad{Msg: "у источника " + source + " нет закреплённой карточки (tg updates — какие есть)"}
	}
	return done, nil
}

// EscalateUpstream — агент разобрал изменение, и нужно решение владельца:
// карточка закреплена со звуком, что решить — дописано в неё.
func EscalateUpstream(ctx context.Context, s *config.Settings, source, note string) (*UpstreamCard, error) {
	note = strings.TrimSpace(note)
	if note == "" {
		return nil, &Bad{Msg: "нужно, что именно владельцу решить"}
	}
	cards, err := PinnedUpstream(s)
	if err != nil {
		return nil, err
	}
	for _, c := range cards {
		if c.Source != source {
			continue
		}
		if c.Event != "" {
			if err := resolveUpstreamNotice(ctx, s, source, c.Event, note, false); err != nil {
				return nil, err
			}
			c.Agent = false
			if notices, err := upstreamNotices(s); err == nil {
				if n := notices[source]; n.Event == c.Event {
					c.MessageID, c.CardPending = n.CardID, n.CardPending
				}
			}
			logEvent(s, "upstream_escalated", "source", source, "message_id", c.MessageID)
			return &c, nil
		}
		var pc pinnedCard
		_ = updatePinned(s, func(m map[string]pinnedCard) { pc = m[source] })
		text := pc.Text + "\n\n<b>⚠️ Нужно твоё решение:</b> " + html.EscapeString(truncRunes(note, 800))
		if err := upstreamBot(ctx, s, "editMessageText", map[string]any{
			"chat_id": s.ApprovalChatID, "message_id": c.MessageID, "text": text, "parse_mode": "HTML",
			"link_preview_options": map[string]any{"is_disabled": true}, "reply_markup": upstreamKeyboard(s),
		}, nil); err != nil {
			logEvent(s, "card_edit_failed", "source", source, "message_id", c.MessageID, "error", err.Error())
		}
		// само сообщение пришло без звука — звучит закрепление
		if err := upstreamBot(ctx, s, "pinChatMessage", map[string]any{
			"chat_id": s.ApprovalChatID, "message_id": c.MessageID, "disable_notification": false,
		}, nil); err != nil {
			logEvent(s, "upstream_pin_failed", "source", source, "message_id", c.MessageID, "error", err.Error())
			return nil, fmt.Errorf("не закрепилось: %w", err)
		}
		_ = updatePinned(s, func(m map[string]pinnedCard) {
			if m[source].ID == c.MessageID {
				m[source] = pinnedCard{ID: c.MessageID, Text: text}
			}
		})
		logEvent(s, "upstream_escalated", "source", source, "message_id", c.MessageID)
		c.Agent = false
		return &c, nil
	}
	return nil, &Bad{Msg: "у источника " + source + " нет открытой карточки (tg updates — какие есть)"}
}

// UpstreamThread — тред, который будит слежение за обновлениями
// (tg updates thread): владелец хочет разборы в одном треде агента
// проекта, а не новый тред на каждое изменение (12.11.2026).
type UpstreamThread struct {
	Thread string    `json:"thread"`
	Set    time.Time `json:"set"`
}

func upstreamDB() (*store.DB, error) { return store.Open(config.DataPath()) }

// UpstreamIncident — сбой, который служба другого профиля заметила у того,
// за чем следит слежение (WhatsApp отверг клиент — устарела whatsmeow):
// карточка и разбор — у основного профиля, как у любого изменения
// источника (tg updates — без --profile, «Разобрано» нажимает владелец в
// боте основного).
type UpstreamIncident struct {
	Source string `json:"source"` // id источника (upstream.Sources)
	Card   string `json:"card"`   // карточка, HTML
	Title  string `json:"title"`
	Brief  string `json:"brief"` // агенту: что случилось
}

// ReportIncident — op upstream_incident в основной службе.
func ReportIncident(ctx context.Context, s *config.Settings, in UpstreamIncident) (any, error) {
	if config.Profile() != "" {
		return nil, &Bad{Msg: "карточки обновлений — у основного профиля"}
	}
	known := false
	for _, src := range upstream.Sources() {
		known = known || src.ID == in.Source
	}
	if !known || in.Card == "" {
		return nil, &Bad{Msg: "неизвестный источник: " + in.Source}
	}
	if !s.BotReady() && upstreamWake == nil {
		return nil, &Bad{Msg: "бот подтверждений не настроен"}
	}
	logEvent(s, "upstream_incident", "source", in.Source, "title", in.Title)
	return map[string]bool{"ok": true}, sendPinned(ctx, s, in.Card, in.Source, in.Title, in.Brief)
}

// ReadUpstreamThread — назначенный тред; nil — автоматический разбор не настроен.
func ReadUpstreamThread() *UpstreamThread {
	db, err := upstreamDB()
	if err != nil {
		return nil
	}
	var t UpstreamThread
	if ok, err := store.GetKV(context.Background(), db.Q(), store.KeyUpstreamThread, &t); !ok || err != nil ||
		strings.TrimSpace(t.Thread) == "" {
		return nil
	}
	return &t
}

// SetUpstreamThread — назначить существующий тред для автоматического разбора.
func SetUpstreamThread(thread string) error {
	thread = strings.TrimSpace(thread)
	if thread == "" || thread == "new" {
		return errors.New("укажите существующий тред: tg updates thread here|<id>")
	}
	db, err := upstreamDB()
	if err != nil {
		return err
	}
	ctx := context.Background()
	return db.Write(ctx, func(tx *sql.Tx) error {
		return store.PutKV(ctx, tx, store.KeyUpstreamThread, UpstreamThread{Thread: thread, Set: time.Now()})
	})
}

var nestorSend = func(ctx context.Context, exe, text string, args ...string) error {
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, exe, args...)
	cmd.Stdin = strings.NewReader(text)
	// пишет служба, а не тред: с T3CODE_THREAD_ID (запуск из треда) Нестор
	// не шлёт в «свой собственный тред» — назначенный тред не разбудить
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "T3CODE_THREAD_ID=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	hideWindow(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, truncRunes(strings.TrimSpace(string(out)), 300))
	}
	return nil
}

// wakeProjectAgent — разбудить агента этого проекта через Нестора (почта
// между агентами машины): только в назначенном треде (tg updates thread).
// Нет назначения или доставка не удалась — ошибка, карточка идёт владельцу;
// другой тред автоматически не создаётся.
func wakeProjectAgent(ctx context.Context, s *config.Settings, title, text string) error {
	exe := nestorExe()
	if exe == "" {
		return errors.New("Нестор не найден (TG_NESTOR, PATH, ~/nestor/bin)")
	}
	t := ReadUpstreamThread()
	if t == nil {
		return errors.New("тред разбора не назначен: tg updates thread here|<id>")
	}
	err := nestorSend(ctx, exe, "«"+title+"»\n\n"+text, "send", t.Thread, "-")
	if err != nil && s != nil {
		logEvent(s, "upstream_wake_thread_failed", "thread", t.Thread, "error", err.Error())
	}
	return err
}

func nestorExe() string {
	exe := os.Getenv("TG_NESTOR")
	if exe == "" {
		exe, _ = exec.LookPath("nestor")
	}
	if exe == "" {
		if home, err := os.UserHomeDir(); err == nil {
			if p := filepath.Join(home, "nestor", "bin", "nestor.exe"); fileExists(p) {
				exe = p
			}
		}
	}
	return exe
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
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
