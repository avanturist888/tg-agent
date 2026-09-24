package tgc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/gotd/td/tg"

	"tgagent/internal/config"
	"tgagent/internal/lock"
	"tgagent/internal/omap"
)

// TranscriptCache — расшифровки по ключу «чат:сообщение» в data/transcripts.json.
// Голосовое не меняется, поэтому готовый текст храним навсегда.
type TranscriptCache struct{ Path string }

func (c TranscriptCache) load() map[string]string {
	data := map[string]string{}
	if raw, err := os.ReadFile(c.Path); err == nil {
		_ = json.Unmarshal(raw, &data)
	}
	return data
}

func (c TranscriptCache) Get(key string) (string, bool) {
	v, ok := c.load()[key]
	return v, ok
}

func (c TranscriptCache) Put(key, text string) {
	_ = lock.With(context.Background(), c.Path, 45*time.Second, func() error {
		data := c.load()
		data[key] = text
		raw, err := omap.Marshal(data)
		if err != nil {
			return err
		}
		tmp := c.Path[:len(c.Path)-len(".json")] + ".tmp"
		if err := os.WriteFile(tmp, raw, 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, c.Path)
	})
}

// STTHint — что делать, если Telegram не расшифровал.
const STTHint = "скачай голосовое через tg_download_file и расшифруй своим STT (whisper и т.п.)"

// TranscribeTimeout — сколько ждать финала: ~0,1 с работы на секунду записи,
// с тройным запасом, но не дольше 5 минут.
func TranscribeTimeout(duration *float64) time.Duration {
	d := 60.0
	if duration != nil {
		d = *duration
	}
	return time.Duration(min(300.0, 30.0+0.3*d) * float64(time.Second))
}

// Transcript — результат расшифровки.
type Transcript struct {
	Status  string // done / pending
	Text    string
	Partial string
}

// Transcribe — встроенная расшифровка Telegram (кнопка «→A»).
//
// Telegram отдаёт её частями: сначала pending с растущим текстом, и только
// ответ с pending=false содержит всё. Промежуточный текст — это обрезок,
// выдавать его за расшифровку нельзя.
func (c *Conn) Transcribe(ctx context.Context, t Target, msgID int, timeout time.Duration) (Transcript, error) {
	req := &tg.MessagesTranscribeAudioRequest{Peer: t.Input, MsgID: msgID}
	updates, stop := c.hub.watchTranscribed(msgID)
	defer stop()
	res, err := c.API.MessagesTranscribeAudio(ctx, req)
	if err != nil {
		return Transcript{}, err
	}
	if !res.Pending {
		return Transcript{Status: "done", Text: res.Text}, nil
	}
	latest := res.Text
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		wait := min(5*time.Second, max(100*time.Millisecond, time.Until(deadline)))
		select {
		case <-ctx.Done():
			return Transcript{}, ctx.Err()
		case u := <-updates:
			if len(u.Text) >= len(latest) {
				latest = u.Text
			}
			if !u.Pending {
				return Transcript{Status: "done", Text: u.Text}, nil
			}
		case <-time.After(wait):
			again, err := c.API.MessagesTranscribeAudio(ctx, req)
			if err != nil {
				return Transcript{}, err
			}
			if !again.Pending {
				return Transcript{Status: "done", Text: again.Text}, nil
			}
			if len(again.Text) >= len(latest) {
				latest = again.Text
			}
		}
	}
	return Transcript{Status: "pending", Partial: latest}, nil
}

// ApplyTranscript раскладывает результат по полям так, чтобы обрезок не
// выглядел готовым текстом.
func ApplyTranscript(msg *omap.Map, got Transcript) {
	msg.Set("transcript_status", got.Status)
	if got.Status == "done" {
		msg.Set("transcript", got.Text)
	} else {
		msg.Set("transcript", nil)
	}
	if got.Status == "pending" {
		msg.Set("transcript_partial", got.Partial)
		msg.Set("transcript_hint", "расшифровка ещё не закончена, transcript_partial — только начало. "+
			"Повтори tg_transcribe чуть позже (она ждёт дольше) или "+STTHint)
	}
}

// AttachTranscripts дописывает расшифровку к голосовым и кружкам. Чтение
// чата не должно висеть минутами: ждём в пределах общего бюджета, остальное
// агент доводит через tg_transcribe.
func (c *Conn) AttachTranscripts(ctx context.Context, t Target, rule config.ChatRule, msgs []*omap.Map, cache TranscriptCache, budget int) {
	deadline := time.Now().Add(20 * time.Second)
	for _, msg := range msgs {
		media, _ := msg.Get("media")
		if media != "voice" && media != "video_note" {
			continue
		}
		idv, _ := msg.Get("id")
		id, _ := idv.(int)
		key := fmt.Sprintf("%s:%d", rule.Alias, id)
		if cached, ok := cache.Get(key); ok {
			ApplyTranscript(msg, Transcript{Status: "done", Text: cached})
			continue
		}
		left := time.Until(deadline)
		if budget <= 0 || left < 5*time.Second {
			msg.Set("transcript", nil)
			msg.Set("transcript_status", "not_requested")
			msg.Set("transcript_hint", "не успел расшифровать в этом вызове — вызови tg_transcribe")
			continue
		}
		budget--
		var dur *float64
		if d, ok := msg.Get("duration"); ok {
			if f, ok := d.(float64); ok {
				dur = &f
			}
		}
		got, err := c.Transcribe(ctx, t, id, min(left, TranscribeTimeout(dur)))
		if err != nil {
			msg.Set("transcript", nil)
			msg.Set("transcript_status", "error")
			msg.Set("transcript_error", RPCErrorText(err))
			msg.Set("transcript_hint", "Telegram не расшифровал — "+STTHint)
			continue
		}
		ApplyTranscript(msg, got)
		if got.Status == "done" {
			cache.Put(key, got.Text)
		}
	}
}

// RPCErrorText — ошибка Telegram в виде «RPCError: MSG_VOICE_TOO_LONG».
func RPCErrorText(err error) string {
	if code := RPCMessage(err); code != "" {
		return "RPCError: " + code
	}
	return err.Error()
}
