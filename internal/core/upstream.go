package core

// Слежение за обновлениями того, от чего зависит клиент MAX (internal/upstream):
// служба основного профиля раз в час проверяет веб-клиент MAX, каталог сборок
// Android-клиента и PyMax. Изменение сначала разбирает агент, а не владелец
// (он не передатчик): карточка уходит в бот без звука и закрепления, а агент
// проекта просыпается в новом треде (через Нестора) — проверяет, чинит сам и
// снимает карточку (tg updates ack) или зовёт владельца (tg updates escalate:
// закрепить со звуком). Агента не разбудить — карточка сразу владельцу,
// закреплённая, пока он не нажмёт «Разобрано» (u:ack). Новая карточка
// источника заменяет его прежнюю, ещё не разобранную: та откреплена, без
// кнопки и с пометкой. Изменение, о котором бот не сообщил, не запоминается:
// следующая проверка пришлёт его снова. /updates — проверить сейчас.

import (
	"context"
	"encoding/json"
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
	upstreamWake    = wakeProjectAgent
)

func upstreamPath(s *config.Settings) string { return filepath.Join(s.DataDir(), "upstream.json") }

// pinnedCard — закреплённая, ещё не разобранная карточка источника. Хранится
// отдельно от состояния проверок под своим коротким замком: «Разобрано» не
// ждёт проверку, которая держит upstreamMu, пока ходит в сеть.
type pinnedCard struct {
	ID    int64  `json:"id"`
	Text  string `json:"text"`            // HTML карточки: дописать пометку при замене
	Agent bool   `json:"agent,omitempty"` // разбирает агент: не закреплена
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
				brief := fmt.Sprintf("Слежение за обновлениями MAX не может проверить источник «%s» (%s): "+
					"%d проверок подряд с ошибкой: %s\nИсточник мог переехать или поменять формат — "+
					"почини проверку в internal/upstream.", r.Source.Title, id, upstreamFailNotice, r.Err.Error())
				_ = sendPinned(ctx, s, text, id, "Не удаётся проверить: "+r.Source.Title, brief)
			}
		case r.Changed():
			if s.BotReady() {
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

const upstreamAgentMark = "\n\n<i>🔎 Разбирает агент tg-agent — позовёт, если нужно твоё решение.</i>"

func upstreamKeyboard(s *config.Settings) map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{{
		{"text": "✅ Разобрано", "callback_data": s.CallbackPrefix() + upstreamAck},
	}}}
}

// upstreamBrief — изменение для агента, простым текстом.
func upstreamBrief(r upstream.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Слежение за обновлениями MAX: изменение у источника «%s» (%s).\n", r.Source.Title, r.Source.ID)
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

// sendPinned — карточка изменения с кнопкой «Разобрано». Сначала её
// разбирает агент: карточка без звука и не закреплена. Агента не разбудить —
// закреплена для владельца.
func sendPinned(ctx context.Context, s *config.Settings, text, source, title, brief string) error {
	var sent struct {
		MessageID int64 `json:"message_id"`
	}
	toAgent := upstreamWake != nil && brief != ""
	shown := text
	if toAgent {
		shown += upstreamAgentMark
	}
	if err := upstreamBot(ctx, s, "sendMessage", map[string]any{
		"chat_id": s.ApprovalChatID, "text": shown, "parse_mode": "HTML", "disable_notification": toAgent,
		"link_preview_options": map[string]any{"is_disabled": true}, "reply_markup": upstreamKeyboard(s),
	}, &sent); err != nil {
		logEvent(s, "notify_failed", "what", "upstream", "source", source, "error", err.Error())
		return err
	}
	card := pinnedCard{ID: sent.MessageID, Text: text, Agent: toAgent}
	if toAgent {
		task := brief + "\nКарточка в боте подтверждений (сообщение " + fmt.Sprint(sent.MessageID) + "). " +
			"Проверь, затрагивает ли это клиент MAX (internal/maxc, core/max*.go) и слежение (internal/upstream). " +
			"Нужна правка — сделай её сам по правилам проекта (CLAUDE.md). Потом: внимания владельца не нужно — " +
			"bin\\tg.exe updates ack " + source + " \"<вывод одной-двумя фразами>\"; нужно его решение — " +
			"bin\\tg.exe updates escalate " + source + " \"<что именно решить>\"."
		if err := upstreamWake(ctx, s, title, task); err != nil {
			logEvent(s, "upstream_wake_failed", "source", source, "error", err.Error())
			card.Agent = false
			_ = upstreamBot(ctx, s, "editMessageText", map[string]any{
				"chat_id": s.ApprovalChatID, "message_id": sent.MessageID, "text": text, "parse_mode": "HTML",
				"link_preview_options": map[string]any{"is_disabled": true}, "reply_markup": upstreamKeyboard(s),
			}, nil)
		} else {
			logEvent(s, "upstream_agent_woken", "source", source, "message_id", sent.MessageID)
		}
	}
	if !card.Agent {
		// само сообщение могло прийти без звука (ждали агента) — тогда звучит закрепление
		if err := upstreamBot(ctx, s, "pinChatMessage", map[string]any{
			"chat_id": s.ApprovalChatID, "message_id": sent.MessageID, "disable_notification": !toAgent,
		}, nil); err != nil {
			logEvent(s, "upstream_pin_failed", "source", source, "message_id", sent.MessageID, "error", err.Error())
		}
	}
	var old pinnedCard
	if err := updatePinned(s, func(m map[string]pinnedCard) {
		old = m[source]
		m[source] = card
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
	agent := false // карточку разбирал агент — она и не закреплена
	_ = updatePinned(s, func(m map[string]pinnedCard) {
		for _, c := range m {
			agent = agent || (c.ID == id && c.Agent)
		}
	})
	if err := upstreamBot(ctx, s, "unpinChatMessage", map[string]any{
		"chat_id": s.ApprovalChatID, "message_id": id,
	}, nil); err != nil && !agent {
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

// UpstreamCard — закреплённая, ещё не разобранная карточка (tg updates).
type UpstreamCard struct {
	Source    string `json:"source"`
	Title     string `json:"title"`
	MessageID int64  `json:"message_id"`
	Agent     bool   `json:"agent,omitempty"` // разбирает агент, не закреплена
}

// PinnedUpstream — карточки, которые ждут разбора.
func PinnedUpstream(s *config.Settings) ([]UpstreamCard, error) {
	if config.Profile() != "" {
		return nil, &Bad{Msg: "карточки обновлений — у основного профиля: без --profile"}
	}
	var out []UpstreamCard
	err := updatePinned(s, func(m map[string]pinnedCard) {
		for _, src := range upstreamSources() {
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

// wakeProjectAgent — разбудить агента этого проекта в новом треде через
// Нестора (почта между агентами машины). Нет Нестора — ошибка: карточка
// тогда идёт владельцу.
func wakeProjectAgent(ctx context.Context, _ *config.Settings, title, text string) error {
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
	if exe == "" {
		return errors.New("Нестор не найден (TG_NESTOR, PATH, ~/nestor/bin)")
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, exe, "send", config.Root, "-", "--new", "--title", title)
	cmd.Stdin = strings.NewReader(text)
	hideWindow(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, truncRunes(strings.TrimSpace(string(out)), 300))
	}
	return nil
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
