package maxc

// Поиск, папки, реакции, вложения (загрузка и скачивание) и расшифровка
// голосовых. Формы запросов — из PyMax (api/uploads, api/messages, api/self)
// и схем веб-клиента web.max.ru (поиск 73, аудио 301, расшифровка 202/293 —
// в PyMax их нет).

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	OpChatInfo          uint16 = 48
	OpChatsList         uint16 = 53
	OpChatSearch        uint16 = 68
	OpMsgSearch         uint16 = 73
	OpPhotoUpload       uint16 = 80
	OpVideoUpload       uint16 = 82
	OpVideoPlay         uint16 = 83
	OpFileUpload        uint16 = 87
	OpFileDownload      uint16 = 88
	OpNotifAttach       uint16 = 136
	OpMsgReaction       uint16 = 178
	OpMsgCancelReaction uint16 = 179
	OpTranscribe        uint16 = 202
	OpFoldersGet        uint16 = 272
	OpNotifTranscript   uint16 = 293
	OpAudioPlay         uint16 = 301
)

// Серверы файлов MAX подписаны «Russian Trusted Root CA» (Минцифры), которого
// нет в системном хранилище Windows. Доверяем ему только здесь — в клиенте
// для файлов MAX, — а не системе и не остальному шлюзу.
//
//go:embed russian_trusted_root_ca.crt
var russianRootCA []byte

// HTTP — клиент для загрузки и скачивания вложений MAX.
var HTTP = func() *http.Client {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	pool.AppendCertsFromPEM(russianRootCA)
	return &http.Client{Timeout: 15 * time.Minute, Transport: &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}
}()

// ── поиск, чаты, папки ───────────────────────────────────────────────────

// Search — сообщения чата с текстом query (новые первыми, как отдаёт сервер).
func (c *Conn) Search(ctx context.Context, chatID int64, query string, count int) ([]Message, error) {
	var r struct {
		Result []struct {
			Message Message `json:"message"`
		} `json:"result"`
	}
	if err := c.Invoke(ctx, OpMsgSearch, map[string]any{"chatId": chatID, "query": query, "count": count}, &r); err != nil {
		return nil, err
	}
	out := make([]Message, len(r.Result))
	for i, x := range r.Result {
		out[i] = x.Message
	}
	return out, nil
}

// ChatsInfo — чаты по id.
func (c *Conn) ChatsInfo(ctx context.Context, ids []int64) ([]Chat, error) {
	var r struct {
		Chats []Chat `json:"chats"`
	}
	err := c.Invoke(ctx, OpChatInfo, map[string]any{"chatIds": ids}, &r)
	return r.Chats, err
}

// ChatsBefore — страница списка чатов: те, где события раньше marker (мс Unix).
func (c *Conn) ChatsBefore(ctx context.Context, marker int64) ([]Chat, error) {
	var r struct {
		Chats []Chat `json:"chats"`
	}
	err := c.Invoke(ctx, OpChatsList, map[string]any{"marker": marker}, &r)
	return r.Chats, err
}

// AllChats — все чаты аккаунта, страницами (не больше limit).
func (c *Conn) AllChats(ctx context.Context, limit int) ([]Chat, error) {
	seen := map[int64]bool{}
	var out []Chat
	marker := time.Now().UnixMilli()
	for len(out) < limit {
		page, err := c.ChatsBefore(ctx, marker)
		if err != nil {
			return out, err
		}
		added := 0
		for _, ch := range page {
			if !seen[ch.ID] {
				seen[ch.ID] = true
				out = append(out, ch)
				added++
			}
			if ch.LastEventTime > 0 && ch.LastEventTime < marker {
				marker = ch.LastEventTime
			}
		}
		if added == 0 {
			break
		}
	}
	return out, nil
}

// Folder — папка чатов владельца. Include — чаты, добавленные руками;
// Filters — правила (какие — сервер не описывает, отдаём как есть).
type Folder struct {
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	Include []int64 `json:"include"`
	Exclude []int64 `json:"exclude"`
	Filters []int   `json:"filters"`
}

// AllFolder — системная папка «Все».
const AllFolder = "all.chat.folder"

// Признаки чата для правил папок (как FILTER_* веб-клиента).
const (
	FilterUnread = iota
	FilterRead
	FilterChannel
	FilterChat
	FilterDialog
	FilterOwner
	FilterAdmin
	FilterMuted
	FilterContact
	FilterNotContact
	FilterBot
	FilterNotMuted
	FilterMarkedUnread
	FilterOrg
)

// Matches — попадает ли чат с признаками flags (биты Filter*) в папку: как
// у веб-клиента — тип чата (канал, группа, личка, контакт, бот) должен
// совпасть хоть с одним из правил, остальные правила — все сразу.
func (f Folder) Matches(chatID int64, flags uint64) bool {
	for _, id := range f.Exclude {
		if id == chatID {
			return false
		}
	}
	for _, id := range f.Include {
		if id == chatID {
			return true
		}
	}
	if f.ID == AllFolder {
		return true
	}
	if len(f.Filters) == 0 {
		return false
	}
	var kinds, need uint64
	for _, x := range f.Filters {
		switch x {
		case FilterChannel, FilterChat, FilterDialog, FilterContact, FilterNotContact, FilterBot:
			kinds |= 1 << x
		default:
			need |= 1 << x
		}
	}
	if kinds == 0 {
		kinds = 1<<53 - 1
	}
	return flags&need == need && flags&kinds != 0
}

func (c *Conn) Folders(ctx context.Context) ([]Folder, error) {
	var r struct {
		Folders []Folder `json:"folders"`
		Order   []string `json:"foldersOrder"`
	}
	if err := c.Invoke(ctx, OpFoldersGet, map[string]any{"folderSync": 0}, &r); err != nil {
		return nil, err
	}
	pos := map[string]int{}
	for i, id := range r.Order {
		pos[id] = i + 1
	}
	out := append([]Folder(nil), r.Folders...)
	for i := 1; i < len(out); i++ { // по порядку владельца
		for j := i; j > 0 && rank(pos, out[j].ID) < rank(pos, out[j-1].ID); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// ChatFlags — признаки чата для Matches (беззвучность и ботов сервер в
// списке чатов не отдаёт — их правила не срабатывают).
func ChatFlags(ch Chat, self int64, contacts map[int64]bool) uint64 {
	var n uint64 = 1 << FilterNotMuted
	if ch.NewMessages > 0 {
		n |= 1 << FilterUnread
	} else {
		n |= 1 << FilterRead
	}
	switch ch.Type {
	case "DIALOG":
		n |= 1 << FilterDialog
		other := false
		for id := range ch.Participants {
			if id != self && contacts[id] {
				other = true
			}
		}
		if other {
			n |= 1 << FilterContact
		} else {
			n |= 1 << FilterNotContact
		}
	case "CHAT":
		n |= 1 << FilterChat
	case "CHANNEL":
		n |= 1 << FilterChannel
	}
	if ch.Owner == self && ch.Type != "DIALOG" {
		n |= 1<<FilterOwner | 1<<FilterAdmin
	}
	return n
}

func rank(pos map[string]int, id string) int {
	if p, ok := pos[id]; ok {
		return p
	}
	return 1 << 30
}

// ── реакции ──────────────────────────────────────────────────────────────

type ReactionCount struct {
	Reaction string `json:"reaction"`
	Count    int    `json:"count"`
}

type ReactionInfo struct {
	Total    int             `json:"totalCount"`
	Counters []ReactionCount `json:"counters"`
	Mine     string          `json:"yourReaction"`
}

// React — поставить реакцию-эмодзи; "" — снять свою.
func (c *Conn) React(ctx context.Context, chatID, messageID int64, emoji string) (*ReactionInfo, error) {
	var r struct {
		Info ReactionInfo `json:"reactionInfo"`
	}
	var err error
	if emoji == "" {
		err = c.Invoke(ctx, OpMsgCancelReaction, map[string]any{"chatId": chatID, "messageId": messageID}, &r)
	} else {
		err = c.Invoke(ctx, OpMsgReaction, map[string]any{"chatId": chatID, "messageId": messageID,
			"reaction": map[string]any{"reactionType": "EMOJI", "id": emoji}}, &r)
	}
	if err != nil {
		return nil, err
	}
	return &r.Info, nil
}

// ── загрузка ─────────────────────────────────────────────────────────────

// Attach — вложение для Send (SendOptions.Attaches).
type Attach map[string]any

const attachWait = 2 * time.Minute

func uploadRequest(typ, uploader int) map[string]any {
	return map[string]any{"count": 1, "type": typ, "uploaderType": uploader, "profile": false}
}

// UploadPhoto — картинка, которая уйдёт фото (сервер может пережать).
func (c *Conn) UploadPhoto(ctx context.Context, path string) (Attach, error) {
	var r struct {
		URL string `json:"url"`
	}
	if err := c.Invoke(ctx, OpPhotoUpload, uploadRequest(0, 0), &r); err != nil {
		return nil, err
	}
	if r.URL == "" {
		return nil, errors.New("maxc: сервер не дал адрес для фото")
	}
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	if ext == "jpg" {
		ext = "jpeg"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="file"; filename="image.%s"`, ext)}
	h["Content-Type"] = []string{"image/" + ext}
	part, err := mw.CreatePart(h)
	if err != nil {
		return nil, err
	}
	part.Write(data)
	mw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	raw, err := doHTTP(req)
	if err != nil {
		return nil, err
	}
	var res struct {
		Photos map[string]struct {
			Token string `json:"token"`
		} `json:"photos"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("maxc: ответ загрузки фото: %w", err)
	}
	want := ""
	if u, err := url.Parse(r.URL); err == nil {
		q := u.Query()
		want = q.Get("photos")
		if want == "" {
			want = q.Get("photoIds")
		}
	}
	for id, p := range res.Photos {
		if (want == "" || id == want) && p.Token != "" {
			return Attach{"_type": "PHOTO", "photoToken": p.Token}, nil
		}
	}
	return nil, errors.New("maxc: загрузка фото не вернула токен")
}

type uploadInfo struct {
	Info []struct {
		URL     string `json:"url"`
		FileID  ID     `json:"fileId"`
		VideoID ID     `json:"videoId"`
		Token   string `json:"token"`
	} `json:"info"`
}

// UploadFile — файл документом (без пережатия).
func (c *Conn) UploadFile(ctx context.Context, path string) (Attach, error) {
	var r uploadInfo
	if err := c.Invoke(ctx, OpFileUpload, uploadRequest(0, 0), &r); err != nil {
		return nil, err
	}
	if len(r.Info) == 0 || r.Info[0].URL == "" {
		return nil, errors.New("maxc: сервер не дал адрес для файла")
	}
	in := r.Info[0]
	ready := c.waitFor("file:" + strconv.FormatInt(int64(in.FileID), 10))
	defer c.stopWait("file:" + strconv.FormatInt(int64(in.FileID), 10))
	if err := putRaw(ctx, in.URL, path, false, nil); err != nil {
		return nil, err
	}
	waitReady(ctx, ready)
	return Attach{"_type": "FILE", "fileId": int64(in.FileID)}, nil
}

// UploadVideo — видео (сервер его обработает и покажет в ленте).
func (c *Conn) UploadVideo(ctx context.Context, path string) (Attach, error) {
	var r uploadInfo
	if err := c.Invoke(ctx, OpVideoUpload, uploadRequest(0, 0), &r); err != nil {
		return nil, err
	}
	if len(r.Info) == 0 || r.Info[0].URL == "" {
		return nil, errors.New("maxc: сервер не дал адрес для видео")
	}
	in := r.Info[0]
	key := "video:" + strconv.FormatInt(int64(in.VideoID), 10)
	ready := c.waitFor(key)
	defer c.stopWait(key)
	if err := putRaw(ctx, in.URL, path, true, nil); err != nil {
		return nil, err
	}
	waitReady(ctx, ready)
	return Attach{"_type": "VIDEO", "videoId": int64(in.VideoID), "token": in.Token, "videoType": 0}, nil
}

// UploadVoice — голосовое: duration — длительность.
func (c *Conn) UploadVoice(ctx context.Context, path string, duration time.Duration, ua UserAgent) (Attach, error) {
	var r uploadInfo
	if err := c.Invoke(ctx, OpVideoUpload, uploadRequest(2, 1), &r); err != nil {
		return nil, err
	}
	if len(r.Info) == 0 || r.Info[0].URL == "" {
		return nil, errors.New("maxc: сервер не дал адрес для голосового")
	}
	in := r.Info[0]
	agent := url.QueryEscape(fmt.Sprintf("OKMessages/%s (%s; %s; %s)", ua.AppVersion, ua.OSVersion, ua.DeviceName, ua.Screen))
	if err := putRaw(ctx, in.URL, path, true, map[string]string{
		"Content-Type": "application/octet-stream", "User-Agent": agent}); err != nil {
		return nil, err
	}
	a := Attach{"_type": "AUDIO", "duration": duration.Milliseconds(), "wave": make([]byte, 80)}
	if in.Token != "" {
		a["token"] = in.Token
	} else {
		a["audioId"] = int64(in.VideoID)
	}
	return a, nil
}

// putRaw — тело файла одним POST (так грузят файлы, видео и голосовые).
func putRaw(ctx context.Context, target, path string, bytesPrefix bool, extra map[string]string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	n := st.Size()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, f)
	if err != nil {
		return err
	}
	req.ContentLength = n
	req.Header.Set("Content-Disposition", "attachment; filename="+url.QueryEscape(filepath.Base(path)))
	rng := fmt.Sprintf("0-%d/%d", n-1, n)
	if bytesPrefix {
		rng = "bytes " + rng
	}
	req.Header.Set("Content-Range", rng)
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	_, err = doHTTP(req)
	return err
}

func doHTTP(req *http.Request) ([]byte, error) {
	res, err := HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("maxc: %s: %w", req.URL.Host, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("maxc: %s ответил %d", req.URL.Host, res.StatusCode)
	}
	return raw, nil
}

func waitReady(ctx context.Context, ready <-chan Frame) {
	select {
	case <-ready:
	case <-ctx.Done():
	case <-time.After(attachWait): // не дождались — пробуем отправить; не готово — Send повторит
	}
}

// ── скачивание ───────────────────────────────────────────────────────────

// FileURL — адрес файла-вложения.
func (c *Conn) FileURL(ctx context.Context, chatID, messageID, fileID int64) (string, error) {
	var r struct {
		URL string `json:"url"`
	}
	err := c.Invoke(ctx, OpFileDownload, map[string]any{"chatId": chatID, "messageId": messageID, "fileId": fileID}, &r)
	return r.URL, err
}

// VideoURL — адрес видео в лучшем качестве.
func (c *Conn) VideoURL(ctx context.Context, chatID, messageID, videoID int64) (string, error) {
	var r map[string]any
	if err := c.Invoke(ctx, OpVideoPlay, map[string]any{"chatId": chatID, "messageId": messageID, "videoId": videoID}, &r); err != nil {
		return "", err
	}
	best, bestQ := "", -1
	for k, v := range r {
		s, _ := v.(string)
		if q, ok := strings.CutPrefix(k, "MP4_"); ok && s != "" {
			if n, _ := strconv.Atoi(q); n > bestQ {
				best, bestQ = s, n
			}
		}
	}
	if best == "" {
		for _, k := range []string{"url", "dynamicUrl"} {
			if s, _ := r[k].(string); s != "" {
				return s, nil
			}
		}
		return "", errors.New("maxc: у видео нет адреса для скачивания")
	}
	return best, nil
}

// AudioURL — адрес голосового (mp3, m4a или opus — что есть).
func (c *Conn) AudioURL(ctx context.Context, chatID, messageID, audioID int64, token string) (string, error) {
	req := map[string]any{"audioId": audioID, "chatId": chatID, "messageId": messageID}
	if token != "" {
		req["token"] = token
	}
	var r struct {
		Opus string `json:"opus"`
		M4A  string `json:"m4a"`
		MP3  string `json:"mp3"`
	}
	if err := c.Invoke(ctx, OpAudioPlay, req, &r); err != nil {
		return "", err
	}
	for _, u := range []string{r.MP3, r.M4A, r.Opus} {
		if u != "" {
			return u, nil
		}
	}
	return "", errors.New("maxc: у голосового нет адреса для скачивания")
}

// ErrTooLarge — файл больше разрешённого.
var ErrTooLarge = errors.New("maxc: файл больше предела")

// Download — скачать адрес в файл, не больше limit байт.
func Download(ctx context.Context, src, dst string, limit int64) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return 0, err
	}
	res, err := HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("maxc: скачивание: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return 0, fmt.Errorf("maxc: скачивание: ответ %d", res.StatusCode)
	}
	if limit > 0 && res.ContentLength > limit {
		return 0, ErrTooLarge
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, err
	}
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, io.LimitReader(res.Body, limit+1))
	f.Close()
	if err == nil && limit > 0 && n > limit {
		err = ErrTooLarge
	}
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return n, os.Rename(tmp, dst)
}

// ── расшифровка голосовых ────────────────────────────────────────────────

// Transcript — расшифровка сервера MAX: status SUCCESS, PROCESSING, FAILED,
// NOT_SUPPORTED, MEDIA_NOT_READY…
type Transcript struct {
	Text   string
	Status string
}

var transcriptStatus = map[int64]string{0: "SUCCESS", 1: "PROCESSING", 2: "FAILED", 3: "NOT_SUPPORTED"}

// Transcribe — попросить сервер расшифровать голосовое и дождаться текста
// (не дольше wait).
func (c *Conn) Transcribe(ctx context.Context, chatID, messageID, audioID int64, wait time.Duration) (Transcript, error) {
	key := fmt.Sprintf("transcript:%d:%d", chatID, messageID)
	ready := c.waitFor(key)
	defer c.stopWait(key)
	var r struct {
		Text   string `json:"transcription"`
		Status any    `json:"transcriptionStatus"`
	}
	if err := c.Invoke(ctx, OpTranscribe, map[string]any{"mediaId": audioID, "messageId": messageID, "chatId": chatID}, &r); err != nil {
		return Transcript{}, err
	}
	t := Transcript{Text: r.Text, Status: statusName(r.Status)}
	if t.Status != "PROCESSING" && t.Status != "MEDIA_NOT_READY" {
		return t, nil
	}
	select {
	case f := <-ready:
		var ev struct {
			Text   string `json:"transcription"`
			Status any    `json:"transcriptionStatus"`
		}
		_ = f.Decode(&ev)
		return Transcript{Text: ev.Text, Status: statusName(ev.Status)}, nil
	case <-time.After(wait):
		return t, nil
	case <-ctx.Done():
		return t, ctx.Err()
	}
}

func statusName(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case int64:
		return transcriptStatus[v]
	case int8:
		return transcriptStatus[int64(v)]
	case uint8:
		return transcriptStatus[int64(v)]
	}
	return fmt.Sprint(v)
}
