package tgc

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"html"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/gotd/td/tg"

	"tgagent/internal/omap"
)

// Виджеты (HTML-сообщения, layer 233, docs/tgw-format.md в
// TelegramMessenger/widget-sandbox): мини-страница HTML+CSS+JS, которую
// присылает бот. Сервер хранит её сжатой zlib (RFC 1950), не больше 4 КБ
// сжатой и 32 КБ исходной. Отправить виджет может только бот — с аккаунта
// пользователя INPUT_MEDIA_WIDGET_NOT_ALLOWED, поэтому здесь только чтение.

// maxWidgetHTML — предел распаковки: сервер не пускает больше 32 КБ,
// с запасом на случай, если предел поднимут.
const maxWidgetHTML = 256 << 10

// WidgetOf — виджет сообщения или nil.
func WidgetOf(m *tg.Message) *tg.MessageMediaWidget {
	w, _ := m.Media.(*tg.MessageMediaWidget)
	return w
}

// WidgetHTML — исходный HTML виджета.
func WidgetHTML(w *tg.MessageMediaWidget) (string, error) {
	r, err := zlib.NewReader(bytes.NewReader(w.Payload))
	if err != nil {
		return "", fmt.Errorf("виджет не распаковать: %w", err)
	}
	defer r.Close()
	raw, err := io.ReadAll(io.LimitReader(r, maxWidgetHTML+1))
	if err != nil {
		return "", fmt.Errorf("виджет не распаковать: %w", err)
	}
	if len(raw) > maxWidgetHTML {
		return "", fmt.Errorf("виджет больше %d КБ", maxWidgetHTML>>10)
	}
	if !utf8.Valid(raw) {
		return "", fmt.Errorf("HTML виджета — не UTF-8")
	}
	return string(raw), nil
}

var (
	widgetTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	widgetDrop  = regexp.MustCompile(`(?is)<(script|style|title|head)\b[^>]*>.*?</(script|style|title|head)>|<!--.*?-->`)
	widgetTag   = regexp.MustCompile(`(?s)<[^>]*>`)
	widgetSpace = regexp.MustCompile(`\s+`)
)

// widgetExcerpt — сколько видимого текста виджета показываем в чтении.
const widgetExcerpt = 300

// widgetText — заголовок и видимый текст разметки (без скриптов и стилей):
// что увидит человек до того, как скрипт что-то нарисует.
func widgetText(src string) (title, text string) {
	if m := widgetTitle.FindStringSubmatch(src); m != nil {
		title = strings.TrimSpace(html.UnescapeString(widgetSpace.ReplaceAllString(m[1], " ")))
	}
	body := widgetDrop.ReplaceAllString(src, " ")
	body = widgetTag.ReplaceAllString(body, " ")
	text = strings.TrimSpace(widgetSpace.ReplaceAllString(html.UnescapeString(body), " "))
	if r := []rune(text); len(r) > widgetExcerpt {
		text = string(r[:widgetExcerpt]) + "…"
	}
	return title, text
}

// WidgetInfo — виджет для агента в чтении: размеры, заголовок, видимый
// текст и состояние. Сам HTML — tg_view_media.
func WidgetInfo(w *tg.MessageMediaWidget) *omap.Map {
	out := omap.New().Set("width", nilZero(w.Width)).Set("height", nilZero(w.Height))
	src, err := WidgetHTML(w)
	if err != nil {
		return out.Set("error", err.Error())
	}
	title, text := widgetText(src)
	if title != "" {
		out.Set("title", title)
	}
	if text != "" {
		out.Set("text", text)
	}
	out.Set("html_bytes", len(src)).Set("script", strings.Contains(strings.ToLower(src), "<script"))
	if w.PublicData != "" {
		out.Set("public_data", w.PublicData) // состояние от бота, видно везде
	}
	if w.ChatData != "" {
		out.Set("chat_data", w.ChatData) // состояние этого сообщения в этом чате
	}
	if w.IsCopy {
		out.Set("copy", true) // пересланная копия: состояние заморожено
	}
	if w.CanQuery {
		out.Set("talks_to_bot", true) // нажатия в виджете уходят боту
	}
	if w.WidgetID != 0 {
		out.Set("widget_id", w.WidgetID) // общий виджет: одно состояние на все копии
	}
	return out
}

// nilZero — 0 у размера виджета значит «по содержимому».
func nilZero(v int) any {
	if v == 0 {
		return "auto"
	}
	return v
}
