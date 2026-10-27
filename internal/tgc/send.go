package tgc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"tgagent/internal/omap"
)

func randomID() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return int64(binary.LittleEndian.Uint64(b[:]))
}

// sent — id и дата отправленных сообщений из ответа Telegram, в порядке randomIDs.
func sent(u tg.UpdatesClass, randomIDs []int64) (ids []int, date int) {
	byRandom := map[int64]int{}
	var fallback []int
	collect := func(list []tg.UpdateClass) {
		for _, up := range list {
			switch v := up.(type) {
			case *tg.UpdateMessageID:
				byRandom[v.RandomID] = v.ID
			case *tg.UpdateNewMessage:
				fallback = append(fallback, v.Message.GetID())
				date = max(date, messageDate(v.Message))
			case *tg.UpdateNewChannelMessage:
				fallback = append(fallback, v.Message.GetID())
				date = max(date, messageDate(v.Message))
			}
		}
	}
	switch v := u.(type) {
	case *tg.UpdateShortSentMessage:
		return []int{v.ID}, v.Date
	case *tg.Updates:
		collect(v.Updates)
	case *tg.UpdatesCombined:
		collect(v.Updates)
	case *tg.UpdateShort:
		collect([]tg.UpdateClass{v.Update})
	}
	for _, r := range randomIDs {
		if id, ok := byRandom[r]; ok {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		ids = fallback
	}
	return ids, date
}

// replyTo — ответ на id; в теме форума (topic > 1) — в этой теме: новое
// сообщение — ответом на саму тему, ответ — с top_msg_id (см. topics.go).
func replyTo(id *int64, topic int) tg.InputReplyToClass {
	msg := 0
	if id != nil {
		msg = int(*id)
	}
	switch {
	case topic > GeneralTopic && (msg == 0 || msg == topic):
		return &tg.InputReplyToMessage{ReplyToMsgID: topic}
	case topic > GeneralTopic:
		return &tg.InputReplyToMessage{ReplyToMsgID: msg, TopMsgID: topic}
	case msg != 0:
		return &tg.InputReplyToMessage{ReplyToMsgID: msg}
	}
	return nil
}

// SendMessage — отправить от аккаунта владельца.
//
// markdown уходит как rich message: заголовки, списки, таблицы, цитаты,
// разделители Telegram отрисовывает сам. Если сервер такое не примет,
// откатываемся на обычное сообщение с классической markdown-разметкой —
// сообщение важнее красоты.
func (c *Conn) SendMessage(ctx context.Context, t Target, text string, reply *int64, format string) (*omap.Map, error) {
	send := func(req *tg.MessagesSendMessageRequest) (int, int, error) {
		req.Peer = t.Input
		req.RandomID = randomID()
		req.NoWebpage = true
		if r := replyTo(reply, t.Topic); r != nil {
			req.ReplyTo = r
		}
		res, err := c.API.MessagesSendMessage(ctx, req)
		if err != nil {
			return 0, 0, err
		}
		ids, date := sent(res, []int64{req.RandomID})
		if len(ids) == 0 {
			return 0, 0, fmt.Errorf("Telegram не вернул id отправленного сообщения")
		}
		return ids[0], date, nil
	}
	result := func(id, date int, format string) *omap.Map {
		var d any
		if date != 0 {
			d = isoTime(date)
		}
		return omap.New().Set("message_id", id).Set("date", d).Set("format", format)
	}

	if format == "markdown" {
		var reason string
		rich, err := c.richInput(ctx, text)
		if err == nil {
			req := &tg.MessagesSendMessageRequest{Message: ""}
			req.SetRichMessage(rich)
			id, date, err := send(req)
			if err == nil {
				return result(id, date, "rich_markdown"), nil
			}
			if _, ok := tgerr.As(err); !ok {
				return nil, err // сеть, отмена — не повод слать второй раз
			}
			reason = RPCErrorText(err)
		} else {
			if ctx.Err() != nil {
				return nil, err
			}
			// не разобрали упоминания — запасной путь их сохранит
			reason = "упоминания: " + RPCErrorText(err)
		}
		plain, entities := ParseMarkdown(text)
		for i, e := range entities {
			entities[i] = c.mentionEntity(e) // [Имя](tg://user?id=…) — упоминание
		}
		req2 := &tg.MessagesSendMessageRequest{Message: plain}
		if len(entities) > 0 {
			req2.SetEntities(entities)
		}
		id, date, err := send(req2)
		if err != nil {
			return nil, err
		}
		return result(id, date, "markdown_fallback").Set("fallback_reason", reason), nil
	}
	id, date, err := send(&tg.MessagesSendMessageRequest{Message: text})
	if err != nil {
		return nil, err
	}
	return result(id, date, "plain"), nil
}

func mimeOf(path string) string {
	if m := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); m != "" {
		if i := strings.IndexByte(m, ';'); i >= 0 {
			m = m[:i]
		}
		return m
	}
	return "application/octet-stream"
}

// OutFile — файл к отправке: Photo — фотографией (видна прямо в ленте),
// иначе документом без пережатия.
type OutFile struct {
	Path  string
	Photo bool
}

// SendFiles — отправить файлы альбомами до 10 штук: фото одним, документы
// другим — смешивать их в одном альбоме Telegram не даёт. reply получает
// первый альбом.
func (c *Conn) SendFiles(ctx context.Context, t Target, files []OutFile, reply *int64) ([]int, error) {
	var photos, docs, voices []string
	for _, f := range files {
		switch {
		case f.Photo:
			photos = append(photos, f.Path)
		case isVoiceFile(f.Path):
			voices = append(voices, f.Path)
		default:
			docs = append(docs, f.Path)
		}
	}
	var ids []int
	// голосовые в альбом не собираются — каждое отдельным сообщением
	for _, p := range voices {
		got, err := c.sendVoice(ctx, t, p, reply)
		if err != nil {
			return ids, err
		}
		ids = append(ids, got...)
		reply = nil
	}
	for _, g := range []struct {
		paths []string
		photo bool
	}{{photos, true}, {docs, false}} {
		if len(g.paths) == 0 {
			continue
		}
		got, err := c.sendAlbum(ctx, t, g.paths, g.photo, reply)
		if err != nil && g.photo && photoRejected(err) {
			// фото Telegram не принял (размеры, формат) — те же файлы документами
			got, err = c.sendAlbum(ctx, t, g.paths, false, reply)
		}
		if err != nil {
			return ids, err
		}
		ids = append(ids, got...)
		reply = nil
	}
	return ids, nil
}

// isVoiceFile — .ogg/.oga/.opus уходят голосовым: Telegram ждёт там Opus в OGG.
func isVoiceFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ogg", ".oga", ".opus":
		return true
	}
	return false
}

// oggVoice — длительность и waveform голосового из OGG/Opus. Длительность —
// по granule position последней страницы (у Opus она всегда в отсчётах
// 48 кГц). Без waveform клиенты рисуют голосовое пустой полоской, а
// декодера Opus у нас нет — громкость оцениваем по размерам пакетов: в VBR
// тишина занимает единицы байт, речь — десятки.
func oggVoice(path string) (duration int, waveform []byte) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, nil
	}
	var sizes []int
	var granule uint64
	cur := 0
	for i := 0; i+27 <= len(data); {
		if !bytes.Equal(data[i:i+4], []byte("OggS")) {
			break
		}
		nseg := int(data[i+26])
		if i+27+nseg > len(data) {
			break
		}
		if g := binary.LittleEndian.Uint64(data[i+6 : i+14]); g != ^uint64(0) {
			granule = g
		}
		body := 0
		for _, l := range data[i+27 : i+27+nseg] {
			cur += int(l)
			body += int(l)
			if l < 255 {
				sizes = append(sizes, cur)
				cur = 0
			}
		}
		i += 27 + nseg + body
	}
	duration = int((granule + 47999) / 48000)
	if len(sizes) > 2 {
		waveform = packWaveform(levels(sizes[2:], 100)) // первые два — OpusHead и OpusTags
	}
	return duration, waveform
}

// levels — n столбцов 0..31: средний размер пакета в доле записи, от самого
// тихого столбца до самого громкого.
func levels(sizes []int, n int) []int {
	n = min(n, len(sizes))
	avg := make([]float64, n)
	for k := range n {
		part := sizes[k*len(sizes)/n : (k+1)*len(sizes)/n]
		for _, s := range part {
			avg[k] += float64(s)
		}
		avg[k] /= float64(len(part))
	}
	lo, hi := slices.Min(avg), slices.Max(avg)
	out := make([]int, n)
	for k, a := range avg {
		if hi > lo {
			out[k] = int(31 * (a - lo) / (hi - lo))
		}
	}
	return out
}

// packWaveform — значения по 5 бит подряд, младшие биты первыми (как
// пишут waveform официальные клиенты).
func packWaveform(v []int) []byte {
	out := make([]byte, (len(v)*5+7)/8)
	for i, x := range v {
		bit := i * 5
		w := uint16(x&31) << (bit % 8)
		out[bit/8] |= byte(w)
		if bit/8+1 < len(out) {
			out[bit/8+1] |= byte(w >> 8)
		}
	}
	return out
}

func (c *Conn) sendVoice(ctx context.Context, t Target, path string, reply *int64) ([]int, error) {
	file, err := uploader.NewUploader(c.API).FromPath(ctx, path)
	if err != nil {
		return nil, err
	}
	dur, wave := oggVoice(path)
	audio := &tg.DocumentAttributeAudio{Voice: true, Duration: dur}
	if wave != nil {
		audio.SetWaveform(wave)
	}
	req := &tg.MessagesSendMediaRequest{
		Peer: t.Input,
		Media: &tg.InputMediaUploadedDocument{
			File:       file,
			MimeType:   "audio/ogg",
			Attributes: []tg.DocumentAttributeClass{audio},
		},
		RandomID: randomID(),
	}
	if r := replyTo(reply, t.Topic); r != nil {
		req.ReplyTo = r
	}
	res, err := c.API.MessagesSendMedia(ctx, req)
	if err != nil {
		return nil, err
	}
	ids, _ := sent(res, []int64{req.RandomID})
	return ids, nil
}

func photoRejected(err error) bool {
	e, ok := tgerr.As(err)
	return ok && (strings.HasPrefix(e.Type, "PHOTO_") || e.Type == "IMAGE_PROCESS_FAILED")
}

// sendAlbum — одно сообщение или альбом: все фото или все документы.
func (c *Conn) sendAlbum(ctx context.Context, t Target, paths []string, photo bool, reply *int64) ([]int, error) {
	up := uploader.NewUploader(c.API)
	var media []tg.InputMediaClass
	for _, p := range paths {
		file, err := up.FromPath(ctx, p)
		if err != nil {
			return nil, err
		}
		if photo {
			media = append(media, &tg.InputMediaUploadedPhoto{File: file})
			continue
		}
		media = append(media, &tg.InputMediaUploadedDocument{
			File:       file,
			MimeType:   mimeOf(p),
			Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: filepath.Base(p)}},
			ForceFile:  true,
		})
	}
	if len(media) == 1 {
		req := &tg.MessagesSendMediaRequest{Peer: t.Input, Media: media[0], RandomID: randomID()}
		if r := replyTo(reply, t.Topic); r != nil {
			req.ReplyTo = r
		}
		res, err := c.API.MessagesSendMedia(ctx, req)
		if err != nil {
			return nil, err
		}
		ids, _ := sent(res, []int64{req.RandomID})
		return ids, nil
	}
	// альбом: файлы сначала загружаем как медиа, потом шлём одним запросом
	var single []tg.InputSingleMedia
	var randoms []int64
	for _, m := range media {
		got, err := c.API.MessagesUploadMedia(ctx, &tg.MessagesUploadMediaRequest{Peer: t.Input, Media: m})
		if err != nil {
			return nil, err
		}
		in, err := uploadedMedia(got)
		if err != nil {
			return nil, err
		}
		r := randomID()
		randoms = append(randoms, r)
		single = append(single, tg.InputSingleMedia{Media: in, RandomID: r})
	}
	req := &tg.MessagesSendMultiMediaRequest{Peer: t.Input, MultiMedia: single}
	if r := replyTo(reply, t.Topic); r != nil {
		req.ReplyTo = r
	}
	res, err := c.API.MessagesSendMultiMedia(ctx, req)
	if err != nil {
		return nil, err
	}
	ids, _ := sent(res, randoms)
	return ids, nil
}

// uploadedMedia — загруженное фото или документ как ссылка для альбома.
func uploadedMedia(got tg.MessageMediaClass) (tg.InputMediaClass, error) {
	switch m := got.(type) {
	case *tg.MessageMediaPhoto:
		if p, ok := m.Photo.(*tg.Photo); ok {
			return &tg.InputMediaPhoto{ID: &tg.InputPhoto{ID: p.ID, AccessHash: p.AccessHash, FileReference: p.FileReference}}, nil
		}
	case *tg.MessageMediaDocument:
		if d, ok := m.Document.(*tg.Document); ok {
			return &tg.InputMediaDocument{ID: &tg.InputDocument{ID: d.ID, AccessHash: d.AccessHash, FileReference: d.FileReference}}, nil
		}
	}
	return nil, fmt.Errorf("Telegram вернул неожиданное медиа (%s)", typeName(got))
}

// NormalizeEmoji — Telegram хранит реакции без селектора вариации: «❤», а не
// «❤️». Агенты обычно пишут с ним — без нормализации сервер отвечает
// REACTION_INVALID.
func NormalizeEmoji(e string) string {
	return strings.TrimSpace(strings.ReplaceAll(e, "️", ""))
}

// AvailableReactions — активные стандартные реакции, живой список от Telegram.
func (c *Conn) AvailableReactions(ctx context.Context) ([]string, error) {
	res, err := c.API.MessagesGetAvailableReactions(ctx, 0)
	if err != nil {
		return nil, err
	}
	list, ok := res.(*tg.MessagesAvailableReactions)
	if !ok {
		return nil, nil
	}
	var out []string
	for _, r := range list.Reactions {
		if !r.Inactive {
			out = append(out, r.Reaction)
		}
	}
	return out, nil
}

// SetReaction — поставить реакцию от аккаунта владельца (emoji == "" — снять свою).
func (c *Conn) SetReaction(ctx context.Context, t Target, msgID int, emoji string) ([]*omap.Map, error) {
	var reaction []tg.ReactionClass
	if emoji != "" {
		emoji = NormalizeEmoji(emoji)
		allowed, err := c.AvailableReactions(ctx)
		if err != nil {
			return nil, err
		}
		if len(allowed) > 0 && !contains(allowed, emoji) {
			return nil, &Bad{fmt.Sprintf("«%s» нет среди реакций Telegram. Доступны: %s", emoji, strings.Join(allowed, " "))}
		}
		reaction = []tg.ReactionClass{&tg.ReactionEmoji{Emoticon: emoji}}
	}
	req := &tg.MessagesSendReactionRequest{Peer: t.Input, MsgID: msgID}
	req.SetReaction(reaction)
	if _, err := c.API.MessagesSendReaction(ctx, req); err != nil {
		return nil, err
	}
	m, _, err := c.Message(ctx, t, msgID)
	if err != nil || m == nil {
		return nil, nil
	}
	if msg, ok := m.(*tg.Message); ok {
		return Reactions(msg), nil
	}
	return nil, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// VoiceDuration — длительность голосового OGG/Opus в секундах (0 — не вышло).
func VoiceDuration(path string) int {
	d, _ := oggVoice(path)
	return d
}
