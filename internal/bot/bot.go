// Package bot — клиент Bot API: карточки подтверждения и кнопки под ними.
//
// Отдельный канал от пользовательской сессии: черновик показывает бот
// уведомлений, а отправляет уже аккаунт владельца.
package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tgagent/internal/attach"
	"tgagent/internal/audit"
	"tgagent/internal/config"
	"tgagent/internal/outbox"
)

const api = "https://api.telegram.org"

// UploadLimit — Bot API не принимает файлы больше.
const UploadLimit = 50 * 1024 * 1024

// Error — отказ Bot API или сети.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func client(proxy string, timeout time.Duration) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = (&net.Dialer{Timeout: 15 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = 15 * time.Second
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, err
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: tr, Timeout: timeout}, nil
}

func routes(s *config.Settings) []string {
	r := []string{""}
	if s.BotProxy != "" {
		r = append(r, s.BotProxy)
	}
	return r
}

type reply struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
}

// Call — вызов Bot API: сначала напрямую, при сетевом отказе — через прокси.
// Провайдер может резать api.telegram.org, а туннель умеет молча протухать.
func Call(ctx context.Context, s *config.Settings, method string, payload any, out any) error {
	if s.BotToken == "" {
		return &Error{"Не задан токен бота подтверждений."}
	}
	if payload == nil {
		payload = map[string]any{}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/bot%s/%s", api, s.BotToken, method)
	var last error
	for _, proxy := range routes(s) {
		for attempt := 1; attempt <= 2; attempt++ { // канал джиттерит, одна осечка — не отказ
			hc, err := client(proxy, 75*time.Second)
			if err != nil {
				return err
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := hc.Do(req)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				last = scrub(err, s)
				if attempt == 1 {
					sleep(ctx, 1500*time.Millisecond)
				}
				continue
			}
			return decode(resp, method, out)
		}
	}
	return &Error{fmt.Sprintf("%s: сеть недоступна ни напрямую, ни через прокси (%v)", method, last)}
}

func decode(resp *http.Response, method string, out any) error {
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return &Error{fmt.Sprintf("%s: %v", method, err)}
	}
	var r reply
	if err := json.Unmarshal(raw, &r); err != nil {
		return &Error{fmt.Sprintf("%s: %s", method, strings.TrimSpace(string(raw)))}
	}
	if !r.OK {
		desc := r.Description
		if desc == "" {
			desc = string(raw)
		}
		return &Error{fmt.Sprintf("%s: %s", method, desc)}
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

// scrub убирает токен из текста ошибки: net/http пишет полный URL.
func scrub(err error, s *config.Settings) error {
	if s.BotToken == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), s.BotToken, "<token>"))
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// UploadDocument — sendDocument с файлом из снимка черновика.
func UploadDocument(ctx context.Context, s *config.Settings, path, caption string) (int64, error) {
	if s.BotToken == "" {
		return 0, &Error{"Не задан токен бота подтверждений."}
	}
	endpoint := fmt.Sprintf("%s/bot%s/sendDocument", api, s.BotToken)
	var last error
	for _, proxy := range routes(s) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		_ = w.WriteField("chat_id", fmt.Sprint(s.ApprovalChatID))
		_ = w.WriteField("caption", truncate(caption, 1000))
		_ = w.WriteField("disable_content_type_detection", "true")
		part, err := w.CreateFormFile("document", filepath.Base(path))
		if err != nil {
			return 0, err
		}
		f, err := os.Open(path)
		if err != nil {
			return 0, err
		}
		_, err = io.Copy(part, f)
		f.Close()
		if err != nil {
			return 0, err
		}
		w.Close()
		hc, err := client(proxy, 7*time.Minute)
		if err != nil {
			return 0, err
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
		req.Header.Set("Content-Type", w.FormDataContentType())
		resp, err := hc.Do(req)
		if err != nil {
			last = scrub(err, s)
			continue
		}
		var msg struct {
			MessageID int64 `json:"message_id"`
		}
		if err := decode(resp, "sendDocument", &msg); err != nil {
			return 0, err
		}
		return msg.MessageID, nil
	}
	return 0, &Error{fmt.Sprintf("sendDocument: сеть недоступна (%v)", last)}
}

const mdSpecial = "\\`*_[]()#+-.!|>~=<{}$^"

// MDEscape экранирует служебные символы rich markdown в подставляемых значениях.
func MDEscape(text string) string {
	var b strings.Builder
	for _, ch := range text {
		if strings.ContainsRune(mdSpecial, ch) {
			b.WriteByte('\\')
		}
		b.WriteRune(ch)
	}
	return b.String()
}

// htmlFromStatus — строка статуса в markdown (**жирный**) → HTML для запасной карточки.
func htmlFromStatus(status string) string {
	parts := strings.Split(status, "**")
	var b strings.Builder
	for i, p := range parts {
		if i%2 == 0 {
			b.WriteString(html.EscapeString(p))
		} else {
			b.WriteString("<b>" + html.EscapeString(p) + "</b>")
		}
	}
	return b.String()
}

// Separator — видимая черта текстом: rich-блок Divider Telegram рисует почти незаметно.
var Separator = strings.Repeat("─", 24)

func fileLabel(f outbox.FileSnap, md bool) string {
	name := html.EscapeString(f.Name)
	if md {
		name = MDEscape(f.Name)
	}
	extra := ""
	if f.Size > UploadLimit {
		extra = " — не показан, больше 50 МБ"
	}
	return fmt.Sprintf("%s (%s%s)", name, attach.HumanSize(f.Size), extra)
}

func heading(d *outbox.Draft) string {
	if d.Status == outbox.Scheduled || d.ApprovedBy() == "auto_send" {
		return "Агент отправляет сообщение (автоотправка)"
	}
	return "Агент просит отправить сообщение"
}

// CardMarkdown — карточка: шапка, разделитель, сам текст, отрисованный так, как уйдёт.
func CardMarkdown(d *outbox.Draft, title, note, status string) string {
	head := []string{
		"🤖 **" + heading(d) + "**",
		fmt.Sprintf("Чат: **%s** (`%s`)", MDEscape(title), d.Chat),
	}
	if note != "" {
		head = append(head, "Повод: "+MDEscape(note))
	}
	if d.ReplyTo != nil {
		head = append(head, fmt.Sprintf("Ответом на сообщение #%d", *d.ReplyTo))
	}
	if len(d.Files) > 0 {
		labels := make([]string, len(d.Files))
		for i, f := range d.Files {
			labels[i] = fileLabel(f, true)
		}
		head = append(head, "📎 Файлы: "+strings.Join(labels, ", "))
	}
	body := "*без текста — только файлы*"
	if d.Text != "" {
		body = d.Text
		if d.Fmt != "markdown" {
			body = MDEscape(d.Text)
		}
	}
	card := strings.Join(head, "\n\n") + "\n\n" + Separator + "\n\n" + body
	if status != "" {
		card += "\n\n" + Separator + "\n\n" + status
	}
	return card
}

// CardHTML — запасная карточка на старом HTML, если rich markdown не прошёл.
func CardHTML(d *outbox.Draft, title, note, status string) string {
	head := []string{
		"🤖 <b>" + heading(d) + "</b>",
		fmt.Sprintf("Чат: <b>%s</b> (<code>%s</code>)", html.EscapeString(title), html.EscapeString(d.Chat)),
	}
	if note != "" {
		head = append(head, "Повод: "+html.EscapeString(note))
	}
	if d.ReplyTo != nil {
		head = append(head, fmt.Sprintf("Ответом на сообщение #%d", *d.ReplyTo))
	}
	if len(d.Files) > 0 {
		labels := make([]string, len(d.Files))
		for i, f := range d.Files {
			labels[i] = fileLabel(f, false)
		}
		head = append(head, "📎 Файлы: "+strings.Join(labels, ", "))
	}
	card := strings.Join(head, "\n") + "\n\n<pre>" + html.EscapeString(d.Text) + "</pre>"
	if status != "" {
		card += "\n\n" + htmlFromStatus(status)
	}
	return card
}

// Keyboard — кнопки под карточкой. У автоотправки — «Отправить сейчас»
// (не ждать окна) и «Отменить».
func Keyboard(d *outbox.Draft) map[string]any {
	if d.Status == outbox.Scheduled {
		return map[string]any{"inline_keyboard": [][]map[string]string{{
			{"text": "✅ Отправить сейчас", "callback_data": "d:" + d.ID + ":ok"},
			{"text": "✋ Отменить", "callback_data": "d:" + d.ID + ":no"},
		}}}
	}
	return map[string]any{"inline_keyboard": [][]map[string]string{{
		{"text": "✅ Отправить", "callback_data": "d:" + d.ID + ":ok"},
		{"text": "✋ Отклонить", "callback_data": "d:" + d.ID + ":no"},
	}}}
}

// ScheduledStatus — строка статуса под карточкой автоотправки.
func ScheduledStatus(d *outbox.Draft) string {
	when := "скоро"
	if d.SendAt != nil {
		if at, err := time.Parse(time.RFC3339, *d.SendAt); err == nil {
			when = at.Local().Format("15:04:05")
		}
	}
	return fmt.Sprintf("⏳ **Уйдёт в %s**, если не отменить. Можно отправить сразу.", when)
}

// SendDraftCard показывает черновик в боте и возвращает id карточки.
func SendDraftCard(ctx context.Context, s *config.Settings, d *outbox.Draft, title, note string) (int64, error) {
	// Сначала сами файлы — чтобы было видно, что именно уйдёт, — потом карточка с кнопками
	for _, f := range d.Files {
		if f.Size > UploadLimit {
			continue
		}
		if _, err := UploadDocument(ctx, s, f.Path, fmt.Sprintf("📎 %s — к черновику %s", f.Name, d.ID)); err != nil {
			_ = audit.Log(s.AuditPath(), "card_file_failed", "draft_id", d.ID, "name", f.Name, "error", err.Error())
		}
	}
	status := ""
	if d.Status == outbox.Scheduled {
		status = ScheduledStatus(d)
	}
	var msg struct {
		MessageID int64 `json:"message_id"`
	}
	err := Call(ctx, s, "sendRichMessage", map[string]any{
		"chat_id":      s.ApprovalChatID,
		"reply_markup": Keyboard(d),
		"rich_message": map[string]string{"markdown": CardMarkdown(d, title, note, status)},
	}, &msg)
	if err != nil {
		// markdown агента мог не пройти разбор — карточку всё равно показываем
		_ = audit.Log(s.AuditPath(), "card_rich_failed", "draft_id", d.ID, "error", err.Error())
		err = Call(ctx, s, "sendMessage", map[string]any{
			"chat_id":                  s.ApprovalChatID,
			"reply_markup":             Keyboard(d),
			"text":                     CardHTML(d, title, note, status),
			"parse_mode":               "HTML",
			"disable_web_page_preview": true,
		}, &msg)
		if err != nil {
			return 0, err
		}
	}
	return msg.MessageID, nil
}

// FinishCard переписывает карточку итогом и снимает кнопки, чтобы не нажали
// дважды. Каждая неудача идёт в журнал: молчаливый проглот однажды уже
// оставил кнопки висеть под обработанным черновиком.
func FinishCard(ctx context.Context, s *config.Settings, messageID int64, d *outbox.Draft, title, status, note string) {
	base := func() map[string]any {
		return map[string]any{"chat_id": s.ApprovalChatID, "message_id": messageID}
	}
	rich := base()
	rich["rich_message"] = map[string]string{"markdown": CardMarkdown(d, title, note, status)}
	plain := base()
	plain["text"] = truncate(CardHTML(d, title, note, status), 4000)
	plain["parse_mode"] = "HTML"
	plain["disable_web_page_preview"] = true
	for _, a := range []struct {
		kind    string
		payload map[string]any
	}{{"rich", rich}, {"html", plain}} {
		err := Call(ctx, s, "editMessageText", a.payload, nil)
		if err == nil {
			return
		}
		_ = audit.Log(s.AuditPath(), "card_edit_failed", "message_id", messageID, "kind", a.kind, "error", err.Error())
	}
	if err := Call(ctx, s, "editMessageReplyMarkup", base(), nil); err != nil {
		_ = audit.Log(s.AuditPath(), "card_strip_failed", "message_id", messageID, "error", err.Error())
	}
}

// AnswerCallback — всплывашка после нажатия; не критична.
func AnswerCallback(ctx context.Context, s *config.Settings, callbackID, text string) {
	_ = Call(ctx, s, "answerCallbackQuery", map[string]any{
		"callback_query_id": callbackID, "text": truncate(text, 190),
	}, nil)
}

// Update — апдейт бота (нам нужны только нажатия кнопок).
type Update struct {
	UpdateID      int64          `json:"update_id"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

type CallbackQuery struct {
	ID   string `json:"id"`
	Data string `json:"data"`
	From struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Message *struct {
		MessageID int64 `json:"message_id"`
	} `json:"message"`
}

func GetUpdates(ctx context.Context, s *config.Settings, offset int64, timeout int) ([]Update, error) {
	var ups []Update
	err := Call(ctx, s, "getUpdates", map[string]any{
		"offset": offset, "timeout": timeout, "allowed_updates": []string{"callback_query"},
	}, &ups)
	return ups, err
}

// Me — getMe для doctor.
func Me(ctx context.Context, s *config.Settings) (string, error) {
	var me struct {
		Username string `json:"username"`
	}
	err := Call(ctx, s, "getMe", nil, &me)
	return me.Username, err
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// RefreshCard перерисовывает ещё не решённую карточку, сохраняя кнопки
// (после правки текста владельцем).
func RefreshCard(ctx context.Context, s *config.Settings, messageID int64, d *outbox.Draft, title, note string) error {
	status := ""
	if d.Status == outbox.Scheduled {
		status = ScheduledStatus(d)
	}
	err := Call(ctx, s, "editMessageText", map[string]any{
		"chat_id":      s.ApprovalChatID,
		"message_id":   messageID,
		"rich_message": map[string]string{"markdown": CardMarkdown(d, title, note, status)},
		"reply_markup": Keyboard(d),
	}, nil)
	if err == nil {
		return nil
	}
	return Call(ctx, s, "editMessageText", map[string]any{
		"chat_id":                  s.ApprovalChatID,
		"message_id":               messageID,
		"text":                     truncate(CardHTML(d, title, note, status), 4000),
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
		"reply_markup":             Keyboard(d),
	}, nil)
}
