package wac

import (
	"fmt"
	"regexp"
	"strings"

	"go.mau.fi/whatsmeow/proto/waE2E"
)

// Content — что в сообщении для агента: текст (или подпись), вид вложения
// словами Telegram и цитата.
type Content struct {
	Text    string
	Kind    string
	Context *waE2E.ContextInfo
}

// Parse — содержимое сообщения. Пустое (служебное: ключи, настройки) —
// Kind и Text пусты.
func Parse(m *waE2E.Message) Content {
	if m == nil {
		return Content{}
	}
	switch {
	case m.Conversation != nil:
		return Content{Text: m.GetConversation()}
	case m.ExtendedTextMessage != nil:
		e := m.GetExtendedTextMessage()
		return Content{Text: e.GetText(), Context: e.GetContextInfo()}
	case m.ImageMessage != nil:
		e := m.GetImageMessage()
		return Content{Text: e.GetCaption(), Kind: "photo", Context: e.GetContextInfo()}
	case m.PtvMessage != nil:
		e := m.GetPtvMessage()
		return Content{Text: e.GetCaption(), Kind: "video_note", Context: e.GetContextInfo()}
	case m.VideoMessage != nil:
		e := m.GetVideoMessage()
		kind := "video"
		if e.GetGifPlayback() {
			kind = "animation"
		}
		return Content{Text: e.GetCaption(), Kind: kind, Context: e.GetContextInfo()}
	case m.AudioMessage != nil:
		e := m.GetAudioMessage()
		kind := "audio"
		if e.GetPTT() {
			kind = "voice"
		}
		return Content{Kind: kind, Context: e.GetContextInfo()}
	case m.DocumentMessage != nil:
		e := m.GetDocumentMessage()
		return Content{Text: e.GetCaption(), Kind: "document", Context: e.GetContextInfo()}
	case m.DocumentWithCaptionMessage != nil:
		return Parse(m.GetDocumentWithCaptionMessage().GetMessage())
	case m.StickerMessage != nil:
		return Content{Kind: "sticker", Context: m.GetStickerMessage().GetContextInfo()}
	case m.LocationMessage != nil:
		e := m.GetLocationMessage()
		text := fmt.Sprintf("%.6f, %.6f", e.GetDegreesLatitude(), e.GetDegreesLongitude())
		if n := strings.TrimSpace(e.GetName() + " " + e.GetAddress()); n != "" {
			text = n + " (" + text + ")"
		}
		return Content{Text: text, Kind: "location", Context: e.GetContextInfo()}
	case m.LiveLocationMessage != nil:
		e := m.GetLiveLocationMessage()
		return Content{Text: fmt.Sprintf("%.6f, %.6f", e.GetDegreesLatitude(), e.GetDegreesLongitude()), Kind: "live_location",
			Context: e.GetContextInfo()}
	case m.ContactMessage != nil:
		e := m.GetContactMessage()
		return Content{Text: e.GetDisplayName() + "\n" + e.GetVcard(), Kind: "contact", Context: e.GetContextInfo()}
	case m.ContactsArrayMessage != nil:
		e := m.GetContactsArrayMessage()
		var names []string
		for _, c := range e.GetContacts() {
			names = append(names, c.GetDisplayName())
		}
		return Content{Text: strings.Join(names, ", "), Kind: "contact", Context: e.GetContextInfo()}
	}
	if p := pollOf(m); p != nil {
		var opts []string
		for _, o := range p.GetOptions() {
			opts = append(opts, "• "+o.GetOptionName())
		}
		return Content{Text: p.GetName() + "\n" + strings.Join(opts, "\n"), Kind: "poll", Context: p.GetContextInfo()}
	}
	if e := m.GetEventMessage(); e != nil {
		return Content{Text: strings.TrimSpace(e.GetName() + "\n" + e.GetDescription()), Kind: "event", Context: e.GetContextInfo()}
	}
	return Content{}
}

func pollOf(m *waE2E.Message) *waE2E.PollCreationMessage {
	for _, p := range []*waE2E.PollCreationMessage{m.GetPollCreationMessage(), m.GetPollCreationMessageV2(),
		m.GetPollCreationMessageV3(), m.GetPollCreationMessageV5(), m.GetPollCreationMessageV6()} {
		if p != nil {
			return p
		}
	}
	if f := m.GetPollCreationMessageV4(); f != nil {
		return pollOf(f.GetMessage())
	}
	return nil
}

// Empty — в сообщении нечего показать агенту.
func (c Content) Empty() bool { return c.Text == "" && c.Kind == "" }

var (
	reCode    = regexp.MustCompile("(?s)```.*?```|`[^`\n]+`")
	reBold    = regexp.MustCompile(`\*\*(.+?)\*\*|__(.+?)__`)
	reItalic  = regexp.MustCompile(`(^|[^\w*])\*([^*\n]+?)\*`)
	reStrike  = regexp.MustCompile(`~~(.+?)~~`)
	reLink    = regexp.MustCompile(`\[([^\]\n]+)\]\(([^)\s]+)\)`)
	reHeading = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
)

// FromMarkdown — Markdown агента в разметку WhatsApp: **жирный** → *жирный*,
// *курсив* → _курсив_, ~~зачёркнутый~~ → ~зачёркнутый~, [текст](ссылка) →
// «текст (ссылка)», заголовок — жирной строкой. Код остаётся как есть
// (WhatsApp понимает ``` и `).
func FromMarkdown(md string) string {
	var codes []string
	text := reCode.ReplaceAllStringFunc(md, func(c string) string {
		codes = append(codes, c)
		return fmt.Sprintf("\x00%d\x00", len(codes)-1)
	})
	text = reHeading.ReplaceAllString(text, "\x01$1\x01")
	text = reBold.ReplaceAllStringFunc(text, func(s string) string {
		g := reBold.FindStringSubmatch(s)
		return "\x01" + g[1] + g[2] + "\x01"
	})
	text = reItalic.ReplaceAllString(text, "${1}_${2}_")
	text = reStrike.ReplaceAllString(text, "~$1~")
	text = reLink.ReplaceAllStringFunc(text, func(s string) string {
		g := reLink.FindStringSubmatch(s)
		if g[1] == g[2] {
			return g[2]
		}
		return g[1] + " (" + g[2] + ")"
	})
	text = strings.ReplaceAll(text, "\x01", "*")
	for i, c := range codes {
		text = strings.Replace(text, fmt.Sprintf("\x00%d\x00", i), c, 1)
	}
	return text
}
