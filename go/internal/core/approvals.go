package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"tgagent/internal/attach"
	"tgagent/internal/audit"
	"tgagent/internal/bot"
	"tgagent/internal/config"
	"tgagent/internal/lock"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
)

// Подтверждение отправки кнопкой в боте.
//
// Карточка с текстом черновика уходит в бот уведомлений, под ней кнопки.
// Нажатие обрабатывает тот, кто в этот момент держит лок на getUpdates: либо
// демон `tg approvals`, либо сам ожидающий агент. Второго потребителя
// апдейтов Telegram не допускает, поэтому лок — ещё и признак «кто-то слушает».

const pollTimeout = 25

func isFinal(status string) bool {
	return status == outbox.Sent || status == outbox.Cancelled || status == outbox.Expired
}

func readOffset(s *config.Settings) int64 {
	raw, err := os.ReadFile(s.OffsetPath())
	if err != nil {
		return 0
	}
	var v struct {
		Offset int64 `json:"offset"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return 0
	}
	return v.Offset
}

func writeOffset(s *config.Settings, offset int64) {
	_ = os.WriteFile(s.OffsetPath(), []byte(fmt.Sprintf(`{"offset": %d}`, offset)), 0o600)
}

// Notify показывает черновик в боте; 0 — бот молчит.
func Notify(ctx context.Context, s *config.Settings, d *outbox.Draft, title, note string) int64 {
	id, err := bot.SendDraftCard(ctx, s, d, title, note)
	if err != nil {
		_ = audit.Log(s.AuditPath(), "card_failed", "draft_id", d.ID, "error", err.Error())
		return 0
	}
	if _, err := outbox.New(s.OutboxPath()).AttachCard(d.ID, id); err != nil {
		_ = audit.Log(s.AuditPath(), "card_attach_failed", "draft_id", d.ID, "error", err.Error())
	}
	d.BotMessageID = &id
	_ = audit.Log(s.AuditPath(), "card_sent", "draft_id", d.ID, "message_id", id)
	return id
}

func title(s *config.Settings, d *outbox.Draft) string {
	if rule, ok := s.Chats[d.Chat]; ok {
		return rule.Title
	}
	return d.Chat
}

// card переписывает карточку итогом (строка статуса — в markdown).
func card(ctx context.Context, s *config.Settings, cardID *int64, d *outbox.Draft, status string) {
	if cardID != nil && *cardID != 0 {
		bot.FinishCard(ctx, s, *cardID, d, title(s, d), status, d.Note)
	}
}

// deliver — отправить одобренный черновик и вернуть строку итога для карточки.
// Человек уже нажал «Отправить» (или окно отмены закрылось): временные сбои
// переживаем сами, прежде чем сдаваться и звать агента.
func deliver(ctx context.Context, s *config.Settings, draftID, by string) string {
	box := outbox.New(s.OutboxPath())
	var (
		result *omap.Map
		errMsg string
	)
	for attempt := 1; attempt <= 3; attempt++ {
		r, err := DeliverApproved(ctx, s, draftID, by)
		if err == nil {
			result = r
			break
		}
		errMsg = fmt.Sprintf("%s: %v", errName(err), err)
		var d *Denied
		if errors.As(err, &d) { // запрет (чат закрыт, статус не тот) — повтор не поможет
			break
		}
		_ = audit.Log(s.AuditPath(), "send_failed", "draft_id", draftID, "attempt", attempt, "error", err.Error())
		if attempt < 3 {
			select {
			case <-ctx.Done():
				attempt = 3
			case <-time.After(15 * time.Second):
			}
		}
	}
	if result == nil {
		// сбой мог случиться уже после отправки — тогда «Не отправлено» было бы враньём
		if d, err := box.Get(draftID); err == nil && d.Status == outbox.Sent && d.MessageID != nil {
			result = omap.New().Set("sent", true).Set("message_id", *d.MessageID)
		}
	}
	if result == nil {
		if errMsg == "" {
			errMsg = "неизвестная ошибка"
		}
		if _, err := box.MarkSendError(draftID, errMsg); err != nil {
			_ = audit.Log(s.AuditPath(), "mark_error_failed", "draft_id", draftID, "error", err.Error())
		}
		return "⚠️ **Не отправлено** — " + bot.MDEscape(errMsg)
	}
	d, _ := box.Get(draftID)
	t := d.Chat
	if d != nil {
		t = title(s, d)
	}
	mid, _ := result.Get("message_id")
	return fmt.Sprintf("✅ **Отправлено** в «%s» в %s (id %v)", bot.MDEscape(t), time.Now().Format("15:04"), mid)
}

func errName(err error) string {
	var (
		d *Denied
		b *lock.Busy
	)
	switch {
	case errors.As(err, &d):
		return "Denied"
	case errors.As(err, &b):
		return "SessionBusy"
	}
	return "Error"
}

// HandleCallback — нажатие кнопки под карточкой.
func HandleCallback(ctx context.Context, s *config.Settings, q *bot.CallbackQuery) {
	data := q.Data
	if q.From.ID != s.ApprovalChatID {
		bot.AnswerCallback(ctx, s, q.ID, "Эта кнопка не для вас.")
		_ = audit.Log(s.AuditPath(), "callback_rejected", "sender", q.From.ID, "data", data)
		return
	}
	if !strings.HasPrefix(data, "d:") {
		return
	}
	parts := strings.SplitN(data, ":", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	draftID, verdict := parts[1], parts[2]
	box := outbox.New(s.OutboxPath())
	d, err := box.Get(draftID)
	if err != nil {
		bot.AnswerCallback(ctx, s, q.ID, "Черновик не найден.")
		return
	}
	// Под rich-сообщением callback может прийти без message — id карточки и
	// так записан в черновике при её отправке.
	var fromCallback *int64
	if q.Message != nil {
		fromCallback = &q.Message.MessageID
	}
	if !samePtr(fromCallback, d.BotMessageID) {
		_ = audit.Log(s.AuditPath(), "callback_card_mismatch", "draft_id", draftID, "from_callback", fromCallback,
			"stored", d.BotMessageID)
	}
	cardID := d.BotMessageID
	if cardID == nil {
		cardID = fromCallback
	}

	if d.Status == outbox.Scheduled {
		if verdict != "no" {
			bot.AnswerCallback(ctx, s, q.ID, "Уйдёт само по таймеру.")
			return
		}
		if _, err := box.Cancel(draftID, fmt.Sprintf("telegram_button:%d", q.From.ID)); err != nil {
			bot.AnswerCallback(ctx, s, q.ID, "Поздно — уже отправляется.")
			return
		}
		attach.Drop(s, draftID)
		_ = audit.Log(s.AuditPath(), "reject", "draft_id", draftID, "chat", d.Chat, "by", "telegram_button")
		bot.AnswerCallback(ctx, s, q.ID, "Отменено.")
		card(ctx, s, cardID, d, "✋ **Отменено** — сообщение не ушло.")
		return
	}
	if isFinal(d.Status) || d.Status == outbox.Approved {
		bot.AnswerCallback(ctx, s, q.ID, "Уже "+d.Status+".")
		card(ctx, s, cardID, d, fmt.Sprintf("⏹ Уже обработано (%s).", d.Status))
		return
	}
	if d.IsExpired(time.Now()) {
		bot.AnswerCallback(ctx, s, q.ID, "Черновик протух.")
		card(ctx, s, cardID, d, "⌛ **Протухло** — агент готовил это давно.")
		return
	}

	var summary string
	if verdict == "ok" {
		if _, err := box.Approve(draftID, fmt.Sprintf("telegram_button:%d", q.From.ID)); err != nil {
			bot.AnswerCallback(ctx, s, q.ID, err.Error())
			return
		}
		_ = audit.Log(s.AuditPath(), "approve", "draft_id", draftID, "chat", d.Chat, "by", "telegram_button")
		bot.AnswerCallback(ctx, s, q.ID, "Отправляю…")
		// кнопки убираем сразу: отправка может занять секунды, а повторное
		// нажатие за это время сбивает с толку
		card(ctx, s, cardID, d, "⏳ **Отправляю…**")
		summary = deliver(ctx, s, draftID, "telegram_button")
	} else {
		if _, err := box.Cancel(draftID, fmt.Sprintf("telegram_button:%d", q.From.ID)); err != nil {
			bot.AnswerCallback(ctx, s, q.ID, err.Error())
			return
		}
		attach.Drop(s, draftID)
		_ = audit.Log(s.AuditPath(), "reject", "draft_id", draftID, "chat", d.Chat, "by", "telegram_button")
		bot.AnswerCallback(ctx, s, q.ID, "Отклонено.")
		summary = "✋ **Отклонено** — сообщение не ушло."
	}
	card(ctx, s, cardID, d, summary)
}

func samePtr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// CloseCard гасит карточку, если черновик решили мимо кнопок.
func CloseCard(ctx context.Context, s *config.Settings, draftID string, result *omap.Map, summary string) {
	d, err := outbox.New(s.OutboxPath()).Get(draftID)
	if err != nil {
		return
	}
	if summary == "" {
		var mid any
		if result != nil {
			mid, _ = result.Get("message_id")
		}
		summary = fmt.Sprintf("✅ **Отправлено** в %s (id %v)", time.Now().Format("15:04"), mid)
	}
	card(ctx, s, d.BotMessageID, d, summary)
}

// ── автоотправка ─────────────────────────────────────────────────────────

var sendingDue sync.Mutex

// SendDue — отправить запланированные черновики, чьё время пришло. Захват
// идёт под локом очереди, поэтому черновик уйдёт ровно один раз, сколько бы
// процессов ни пытались.
func SendDue(ctx context.Context, s *config.Settings) {
	sendingDue.Lock()
	defer sendingDue.Unlock()
	due, err := outbox.New(s.OutboxPath()).ClaimDue(time.Now())
	if err != nil {
		_ = audit.Log(s.AuditPath(), "auto_send_claim_failed", "error", err.Error())
		return
	}
	for _, d := range due {
		_ = audit.Log(s.AuditPath(), "approve", "draft_id", d.ID, "chat", d.Chat, "by", "auto_send")
		card(ctx, s, d.BotMessageID, d, "⏳ **Отправляю…**")
		summary := deliver(ctx, s, d.ID, "auto_send")
		card(ctx, s, d.BotMessageID, d, summary)
	}
}

// ScheduleLocal — процесс, создавший черновик с автоотправкой, сам дошлёт
// его в срок, если слушателя нет. Слушатель, если есть, успеет раньше или
// позже — захват под локом не даст отправить дважды.
func ScheduleLocal(s *config.Settings, d *outbox.Draft) {
	if d.SendAt == nil {
		return
	}
	at, err := time.Parse(time.RFC3339, *d.SendAt)
	if err != nil {
		return
	}
	go func() {
		time.Sleep(time.Until(at) + 1500*time.Millisecond) // даём слушателю отработать первым
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		SendDue(ctx, s)
	}()
}

// ── разбор апдейтов ──────────────────────────────────────────────────────

// Pump качает апдейты, пока не истечёт время или не сработает stop.
// Вызывается только тем, кто держит лок на getUpdates. Попутно отправляет
// запланированные черновики, чьё время пришло.
func Pump(ctx context.Context, s *config.Settings, until time.Time, stop func() bool) {
	offset := readOffset(s)
	for time.Now().Before(until) {
		if ctx.Err() != nil {
			return
		}
		if stop != nil && stop() {
			return
		}
		SendDue(ctx, s)
		window := int(time.Until(until).Seconds())
		window = max(1, min(pollTimeout, window))
		// ближайшая автоотправка — не ждём дольше неё
		if next, err := outbox.New(s.OutboxPath()).NextDue(); err == nil && next != nil {
			window = max(1, min(window, int(time.Until(*next).Seconds())+1))
		}
		updates, err := bot.GetUpdates(ctx, s, offset, window)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			_ = audit.Log(s.AuditPath(), "poll_failed", "error", err.Error())
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		for _, u := range updates {
			offset = max(offset, u.UpdateID+1)
			writeOffset(s, offset)
			if u.CallbackQuery != nil {
				func() {
					// одно нажатие не должно ронять слушателя
					defer func() {
						if r := recover(); r != nil {
							_ = audit.Log(s.AuditPath(), "callback_failed", "error", fmt.Sprint(r))
						}
					}()
					HandleCallback(ctx, s, u.CallbackQuery)
				}()
			}
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// WaitForDecision — дождаться нажатия кнопки. Работает и с демоном, и без него.
func WaitForDecision(ctx context.Context, s *config.Settings, draftID string, timeout time.Duration) (*omap.Map, error) {
	box := outbox.New(s.OutboxPath())
	deadline := time.Now().Add(timeout)
	resolved := func() bool {
		d, err := box.Get(draftID)
		if err != nil {
			var nf *outbox.NotFound
			return errors.As(err, &nf)
		}
		// одобрено, но отправка сорвалась — агенту надо знать сразу
		return isFinal(d.Status) || (d.Status == outbox.Approved && d.SendError != nil)
	}
	updates := lock.New(s.UpdatesLockPath(), 500*time.Millisecond)
	if s.BotReady() && updates.Acquire(ctx) == nil {
		// слушателя нет — качаем апдейты сами, пока ждём
		Pump(ctx, s, deadline, resolved)
		updates.Release()
	} else {
		// апдейты разбирает демон, нам остаётся следить за статусом
		for time.Now().Before(deadline) && !resolved() && ctx.Err() == nil {
			SendDue(ctx, s) // на случай, если демона нет, а бот не настроен
			sleepCtx(ctx, 2*time.Second)
		}
	}

	d, err := box.Get(draftID)
	if err != nil {
		var nf *outbox.NotFound
		if errors.As(err, &nf) {
			return omap.New().Set("draft_id", draftID).Set("status", "not_found"), nil
		}
		return nil, err
	}
	if d.Status == outbox.Approved && d.SendError != nil {
		return omap.New().Set("draft_id", draftID).Set("chat", d.Chat).Set("status", "send_failed").
			Set("error", *d.SendError).Set("hint",
			"Владелец НАЖАЛ «Отправить», но отправка сорвалась (см. error). Черновик "+
				"одобрен — повтори отправку через tg_send_draft(draft_id), новое "+
				"подтверждение не нужно. Если снова не выйдет, скажи владельцу."), nil
	}
	if d.Status == outbox.Approved {
		return omap.New().Set("draft_id", draftID).Set("chat", d.Chat).Set("status", "sending").
			Set("hint", "Владелец нажал «Отправить», отправка идёт — проверь через tg_list_drafts чуть позже."), nil
	}
	if d.Status == outbox.Scheduled {
		return omap.New().Set("draft_id", draftID).Set("chat", d.Chat).Set("status", "scheduled").
			Set("send_at", d.SendAt).
			Set("hint", "Черновик на автоотправке, время ещё не пришло. Отменить — tg_cancel_draft."), nil
	}
	status := "waiting"
	if isFinal(d.Status) {
		status = d.Status
	}
	var hint string
	switch d.Status {
	case outbox.Sent:
		hint = "Отправлено."
	case outbox.Cancelled:
		hint = "Человек отклонил отправку — не переспрашивай и не пересоздавай черновик без новой просьбы."
	case outbox.Expired:
		hint = "Черновик протух."
	default:
		hint = "Человек пока не нажал кнопку. Карточка в боте жива: сообщи об этом и займись другим."
	}
	return omap.New().Set("draft_id", draftID).Set("chat", d.Chat).Set("status", status).
		Set("message_id", d.MessageID).Set("hint", hint), nil
}

// RecoverInterrupted досылает то, что одобрили кнопкой (или автоотправкой),
// но прошлый слушатель не успел отправить — его убили между нажатием и
// отправкой. В режимах без бота статус approved законно ждёт агента или CLI.
func RecoverInterrupted(ctx context.Context, s *config.Settings) {
	drafts, err := outbox.New(s.OutboxPath()).List(outbox.Approved)
	if err != nil {
		return
	}
	for _, d := range drafts {
		by := d.ApprovedBy()
		byButton := strings.HasPrefix(by, "telegram_button") && s.SendPolicy == "bot_approval"
		if !byButton && by != "auto_send" {
			continue
		}
		if d.SendError != nil {
			continue // уже сорвалось и об этом знают — не долбим повторно
		}
		_ = audit.Log(s.AuditPath(), "recover_interrupted", "draft_id", d.ID, "chat", d.Chat)
		summary := deliver(ctx, s, d.ID, by+" (дослано после перезапуска слушателя)")
		card(ctx, s, d.BotMessageID, d, summary)
	}
}

// RunDaemon — постоянный слушатель кнопок: нажатие срабатывает, даже если агент ушёл.
func RunDaemon(ctx context.Context, s *config.Settings, loader func() (*config.Settings, error)) error {
	fmt.Printf("Слушаю кнопки бота, чат %d. Ctrl+C — выход.\n", s.ApprovalChatID)
	raisePriority()
	// Лок может держать ожидающий агент (пока демона не было, кнопки разбирал
	// он). Выходить нельзя — тогда после ухода агента не останется никого.
	updates := lock.New(s.UpdatesLockPath(), 500*time.Millisecond)
	for {
		err := updates.Acquire(ctx)
		if err == nil {
			break
		}
		if !lock.IsBusy(err) {
			return err
		}
		sleepCtx(ctx, 5*time.Second)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	defer updates.Release()

	pollerCtx, stopPoller := context.WithCancel(ctx)
	defer stopPoller()
	go RunPoller(pollerCtx, loader, time.Duration(s.FeedInterval*float64(time.Second)))

	_ = audit.Log(s.AuditPath(), "daemon_start", "impl", "go")
	RecoverInterrupted(ctx, s)
	for ctx.Err() == nil {
		updates.Touch() // иначе лок сочтут протухшим и перехватят
		if fresh, err := loader(); err == nil {
			s = fresh // белый список и права могли поменяться
		}
		Pump(ctx, s, time.Now().Add(pollTimeout*time.Second), nil)
	}
	return nil
}

// SweepOnce — разобрать накопившиеся нажатия и выйти, если демон не крутится.
func SweepOnce(ctx context.Context, s *config.Settings) (int64, error) {
	before := readOffset(s)
	err := lock.With(ctx, s.UpdatesLockPath(), 5*time.Second, func() error {
		Pump(ctx, s, time.Now().Add(3*time.Second), nil)
		return nil
	})
	return readOffset(s) - before, err
}

// PendingSummary — что ждёт нажатия кнопки.
func PendingSummary(s *config.Settings) ([]*outbox.Draft, error) {
	return outbox.New(s.OutboxPath()).List(outbox.Pending)
}

// ApproveAndSend — одобрить и отправить черновик из GUI (как кнопка в боте).
func ApproveAndSend(ctx context.Context, s *config.Settings, draftID, by string) (string, error) {
	box := outbox.New(s.OutboxPath())
	d, err := box.Get(draftID)
	if err != nil {
		return "", err
	}
	if _, err := sendable(s, d.Chat); err != nil {
		return "", err
	}
	if _, err := box.Approve(draftID, by); err != nil {
		return "", err
	}
	_ = audit.Log(s.AuditPath(), "approve", "draft_id", draftID, "chat", d.Chat, "by", by)
	card(ctx, s, d.BotMessageID, d, "⏳ **Отправляю…**")
	summary := deliver(ctx, s, draftID, by)
	card(ctx, s, d.BotMessageID, d, summary)
	return summary, nil
}

// EditDraft — владелец правит текст черновика до решения (из GUI).
// Карточка в боте перерисовывается, чтобы там был ровно тот текст, что уйдёт.
func EditDraft(ctx context.Context, s *config.Settings, draftID, text, by string) error {
	text = strings.TrimSpace(text)
	if n := len([]rune(text)); n > 32768 {
		return &Bad{fmt.Sprintf("Слишком длинно: %d символов, предел Telegram — 32768.", n)}
	}
	box := outbox.New(s.OutboxPath())
	if d, err := box.Get(draftID); err == nil && text == "" && len(d.Files) == 0 {
		return &Bad{"Пустое сообщение: нужен текст или файлы."}
	}
	d, err := box.EditText(draftID, text, by)
	if err != nil {
		return err
	}
	_ = audit.Log(s.AuditPath(), "edit", "draft_id", draftID, "chat", d.Chat, "by", by, "chars", len([]rune(text)))
	if d.BotMessageID != nil {
		if err := bot.RefreshCard(ctx, s, *d.BotMessageID, d, title(s, d), d.Note); err != nil {
			_ = audit.Log(s.AuditPath(), "card_edit_failed", "message_id", *d.BotMessageID, "kind", "refresh", "error", err.Error())
		}
	}
	return nil
}
