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
// Голосовое не меняется, поэтому готовый текст храним навсегда. Запись —
// {text, engine, language}; старые записи — просто текст Telegram.
type TranscriptCache struct{ Path string }

type cachedTranscript struct {
	Text     string `json:"text"`
	Engine   string `json:"engine"`
	Language string `json:"language,omitempty"`
}

func (c TranscriptCache) load() map[string]json.RawMessage {
	data := map[string]json.RawMessage{}
	if raw, err := os.ReadFile(c.Path); err == nil {
		_ = json.Unmarshal(raw, &data)
	}
	return data
}

// Get — готовая расшифровка из кэша.
func (c TranscriptCache) Get(key string) (Transcript, bool) {
	raw, ok := c.load()[key]
	if !ok {
		return Transcript{}, false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return Transcript{Status: "done", Text: text, Engine: EngineTelegram}, true
	}
	var v cachedTranscript
	if json.Unmarshal(raw, &v) != nil {
		return Transcript{}, false
	}
	return Transcript{Status: "done", Text: v.Text, Engine: v.Engine, Language: v.Language}, true
}

// Put — запомнить готовую расшифровку.
func (c TranscriptCache) Put(key string, got Transcript) {
	_ = lock.With(context.Background(), c.Path, 45*time.Second, func() error {
		data := c.load()
		entry, err := json.Marshal(cachedTranscript{Text: got.Text, Engine: got.Engine, Language: got.Language})
		if err != nil {
			return err
		}
		data[key] = entry
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

// EngineTelegram — встроенная расшифровка Telegram.
const EngineTelegram = "telegram"

// Transcript — результат расшифровки.
type Transcript struct {
	Status  string // done / pending
	Text    string
	Partial string
	// Engine — чем расшифровано: telegram или gigaam-<модель>
	Engine string
	// Language — язык речи, если его определяли (детектор перед GigaAM)
	Language string
	// NoSpeech — речи не слышно (текст пустой не по ошибке)
	NoSpeech bool
	// LocalError — своя расшифровка сорвалась, поэтому Telegram
	LocalError string
}

// Stale — расшифровку стоит переделать: это Telegram, а язык речи никто не
// проверял (запись до своей расшифровки), и теперь есть GigaAM.
func (t Transcript) Stale(o STT) bool {
	return o.Local != nil && t.Engine == EngineTelegram && t.Language == ""
}

// LocalResult — ответ своей расшифровки (GigaAM).
type LocalResult struct {
	Done     bool // расшифровано; false — речь не русская, это дело Telegram
	Text     string
	Engine   string
	Language string
	Speech   bool
}

// STT — своя расшифровка перед Telegram: голосовое скачивается в Dir и
// уходит в Local. Local == nil — только Telegram.
type STT struct {
	Local    func(ctx context.Context, path string) (LocalResult, error)
	Dir      string
	MaxBytes int64
}

// Local — расшифровать своими силами. ok = false — дальше Telegram
// (got.Language/LocalError — почему).
func (c *Conn) Local(ctx context.Context, t Target, msgID int, o STT) (got Transcript, ok bool) {
	if o.Local == nil {
		return Transcript{}, false
	}
	file, err := c.DownloadFile(ctx, t, msgID, o.Dir, o.MaxBytes)
	if err != nil {
		return Transcript{LocalError: "голосовое не скачалось: " + err.Error()}, false
	}
	return LocalFile(ctx, file.Path, o)
}

// LocalFile — то же для уже скачанного файла.
func LocalFile(ctx context.Context, path string, o STT) (Transcript, bool) {
	r, err := o.Local(ctx, path)
	if err != nil {
		return Transcript{LocalError: err.Error()}, false
	}
	if !r.Done {
		return Transcript{Language: r.Language}, false
	}
	return Transcript{Status: "done", Text: r.Text, Engine: r.Engine, Language: r.Language, NoSpeech: !r.Speech}, true
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
	applyEngine(msg, got)
	if got.Status == "pending" {
		msg.Set("transcript_partial", got.Partial)
		msg.Set("transcript_hint", "расшифровка ещё не закончена, transcript_partial — только начало. "+
			"Повтори tg_transcribe чуть позже (она ждёт дольше) или "+STTHint)
	}
}

// applyEngine — чем расшифровано и на каком языке речь.
func applyEngine(msg *omap.Map, got Transcript) {
	if got.Engine != "" {
		msg.Set("transcript_engine", got.Engine)
	}
	if got.Language != "" {
		msg.Set("transcript_language", got.Language)
	}
	if got.NoSpeech {
		msg.Set("transcript_hint", "речи в записи не слышно")
	}
	if got.LocalError != "" {
		msg.Set("transcript_local_error", got.LocalError)
	}
}

// AttachTranscripts дописывает расшифровку к голосовым и кружкам. Чтение
// чата не должно висеть минутами: ждём в пределах общего бюджета, остальное
// агент доводит через tg_transcribe.
func (c *Conn) AttachTranscripts(ctx context.Context, t Target, rule config.ChatRule, msgs []*omap.Map, cache TranscriptCache, budget int, o STT) {
	window := 20 * time.Second
	if o.Local != nil {
		window = time.Minute // своя расшифровка: скачать и прогнать модель
	}
	deadline := time.Now().Add(window)
	for _, msg := range msgs {
		media, _ := msg.Get("media")
		if media != "voice" && media != "video_note" {
			continue
		}
		idv, _ := msg.Get("id")
		id, _ := idv.(int)
		key := fmt.Sprintf("%s:%d", rule.Alias, id)
		if cached, ok := cache.Get(key); ok && !cached.Stale(o) {
			ApplyTranscript(msg, cached)
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
		lctx, cancel := context.WithDeadline(ctx, deadline)
		local, ok := c.Local(lctx, t, id, o)
		cancel()
		if ok {
			ApplyTranscript(msg, local)
			cache.Put(key, local)
			continue
		}
		got, err := c.Transcribe(ctx, t, id, max(time.Second, min(time.Until(deadline), TranscribeTimeout(dur))))
		got.Engine, got.Language, got.LocalError = EngineTelegram, local.Language, local.LocalError
		if err != nil {
			msg.Set("transcript", nil)
			msg.Set("transcript_status", "error")
			msg.Set("transcript_error", RPCErrorText(err))
			msg.Set("transcript_hint", "Telegram не расшифровал — "+STTHint)
			applyEngine(msg, got)
			continue
		}
		ApplyTranscript(msg, got)
		if got.Status == "done" {
			cache.Put(key, got)
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
