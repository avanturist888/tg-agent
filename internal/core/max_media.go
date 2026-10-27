package core

// Аккаунт MAX: поиск, папки и запросы доступа, реакции, вложения (скачать,
// показать, расшифровать, отправить). Карточки, белый список, кэш
// расшифровок и папка downloads — общие с Telegram.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"tgagent/internal/config"
	"tgagent/internal/maxc"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/tgc"
)

// ── поиск ────────────────────────────────────────────────────────────────

func maxSearch(ctx context.Context, c *maxc.Conn, chatID int64, query string, limit int) ([]maxc.Message, error) {
	return c.Search(ctx, chatID, query, limit)
}

// ── папки и запросы доступа ──────────────────────────────────────────────

// maxAllFolder — папка «Все чаты»: в MAX чат может не лежать ни в одной.
const maxAllFolder = "all"

func maxKind(t string) string {
	switch t {
	case "DIALOG":
		return "user"
	case "CHAT":
		return "group"
	case "CHANNEL":
		return "channel"
	}
	return strings.ToLower(t)
}

// maxChatTitle — название чата; у личного — имя собеседника.
func maxChatTitle(ctx context.Context, c *maxc.Conn, ch maxc.Chat) string {
	if ch.Title != "" {
		return ch.Title
	}
	if ch.ID == maxc.SavedChat {
		return "Избранное"
	}
	maxRT.Lock()
	self := maxRT.self
	maxRT.Unlock()
	for id := range ch.Participants {
		if id != self {
			if n := maxName(ctx, c, id); n != "" {
				return n
			}
		}
	}
	return fmt.Sprint(ch.ID)
}

func maxListFolders(ctx context.Context, s *config.Settings) (*omap.Map, error) {
	c, err := maxConn(ctx)
	if err != nil {
		return nil, err
	}
	folders, err := c.Folders(ctx)
	if err != nil {
		return nil, err
	}
	list := []*omap.Map{omap.New().Set("id", maxAllFolder).Set("title", "Все чаты").
		Set("rules", []string{"все чаты аккаунта MAX"})}
	for _, f := range folders {
		if f.ID == maxc.AllFolder {
			continue // то же, что «Все чаты»
		}
		item := omap.New().Set("id", f.ID).Set("title", f.Title).Set("pinned_and_included", len(f.Include))
		if rules := maxFolderRules(f); len(rules) > 0 {
			item.Set("rules", rules)
		}
		list = append(list, item)
	}
	logEvent(s, "folders_listed", "count", len(list))
	return omap.New().Set("folders", list).Set("hint",
		"Чаты папки — tg_folder_chats(folder=<id или название>, account=\""+config.AccountName()+"\")."), nil
}

func maxFolderChats(ctx context.Context, s *config.Settings, folder string) (*omap.Map, error) {
	c, err := maxConn(ctx)
	if err != nil {
		return nil, err
	}
	var (
		chats []maxc.Chat
		title = "Все чаты"
		id    = maxAllFolder
	)
	if strings.EqualFold(folder, maxAllFolder) || strings.EqualFold(folder, title) || folder == maxc.AllFolder {
		if chats, err = c.AllChats(ctx, 1000); err != nil {
			return nil, err
		}
	} else {
		folders, err := c.Folders(ctx)
		if err != nil {
			return nil, err
		}
		names := []string{"all «Все чаты»"}
		var found *maxc.Folder
		for i, f := range folders {
			names = append(names, fmt.Sprintf("%s «%s»", f.ID, f.Title))
			if f.ID == folder || strings.EqualFold(f.Title, folder) {
				found = &folders[i]
			}
		}
		if found == nil {
			return nil, &Bad{Msg: fmt.Sprintf("Папки «%s» нет. Есть: %s.", folder, strings.Join(names, ", "))}
		}
		title, id = found.Title, found.ID
		all, err := c.AllChats(ctx, 1000)
		if err != nil {
			return nil, err
		}
		maxRT.Lock()
		self, contacts := maxRT.self, maxRT.contacts
		maxRT.Unlock()
		for _, ch := range all {
			if found.Matches(ch.ID, maxc.ChatFlags(ch, self, contacts)) {
				chats = append(chats, ch)
			}
		}
	}
	known := map[int64]config.ChatRule{}
	for _, r := range s.Rules() {
		if cid, err := maxChatID(r); err == nil {
			known[cid] = r
		}
	}
	pending := map[int64]string{}
	for _, r := range loadAccess(accessPath(s)) {
		if r.Status == accessPending {
			pending[r.Peer] = r.ID
		}
	}
	slices.SortFunc(chats, func(a, b maxc.Chat) int { return compareInt(b.LastEventTime, a.LastEventTime) })
	rows := make([]*omap.Map, 0, len(chats))
	for _, ch := range chats {
		item := omap.New().Set("id", ch.ID).Set("title", maxChatTitle(ctx, c, ch)).Set("kind", maxKind(ch.Type)).
			Set("unread", ch.NewMessages)
		if rule, ok := known[ch.ID]; ok {
			item.Set("alias", s.Ref(rule.Alias)).Set("can_read", rule.Read).Set("can_send", rule.Send)
		} else {
			item.Set("alias", nil)
		}
		if rid, ok := pending[ch.ID]; ok {
			item.Set("access_request", rid)
		}
		rows = append(rows, item)
	}
	logEvent(s, "folder_chats_listed", "folder", title, "count", len(rows))
	return omap.New().Set("folder", omap.New().Set("id", id).Set("title", title)).
		Set("count", len(rows)).Set("chats", rows).Set("hint",
		"alias = null — чата нет в белом списке. Нужен доступ — tg_request_access(chat_id=id, account=\""+
			config.AccountName()+"\", reason=…)."), nil
}

// maxChatInfo — название и тип чата для карточки запроса доступа.
func maxChatInfo(ctx context.Context, chatID int64) (tgc.DialogRow, error) {
	c, err := maxConn(ctx)
	if err != nil {
		return tgc.DialogRow{}, err
	}
	maxRT.Lock()
	ch, ok := maxRT.chats[chatID]
	maxRT.Unlock()
	if !ok {
		chats, err := c.ChatsInfo(ctx, []int64{chatID})
		if err != nil || len(chats) == 0 {
			return tgc.DialogRow{}, &Bad{Msg: fmt.Sprintf("Чат %d не найден среди чатов аккаунта MAX.", chatID)}
		}
		ch = chats[0]
	}
	return tgc.DialogRow{ID: ch.ID, Title: maxChatTitle(ctx, c, ch), Kind: maxKind(ch.Type), Unread: ch.NewMessages}, nil
}

// maxCatchup — ленты читаемых чатов догоняют историю (после входа и после
// того, как владелец открыл чат).
func maxCatchup(ctx context.Context, s *config.Settings) {
	c, err := maxConn(ctx)
	if err != nil {
		return
	}
	for _, rule := range s.Rules() {
		id, err := maxChatID(rule)
		if !rule.Read || err != nil {
			continue
		}
		hist, err := c.History(ctx, id, 0, 30)
		if err != nil {
			logEvent(s, "max_catchup_failed", "chat", rule.Alias, "error", err.Error())
			continue
		}
		maxFeed(s, rule, hist)
	}
}

// ── реакции ──────────────────────────────────────────────────────────────

func maxReact(ctx context.Context, rule config.ChatRule, messageID int64, emoji string) ([]*omap.Map, error) {
	chatID, err := maxChatID(rule)
	if err != nil {
		return nil, err
	}
	c, err := maxConn(ctx)
	if err != nil {
		return nil, err
	}
	messageID, _ = maxFixID(ctx, c, chatID, messageID)
	info, err := c.React(ctx, chatID, messageID, emoji)
	if err != nil {
		return nil, &Bad{Msg: "MAX отказал: " + err.Error()}
	}
	out := make([]*omap.Map, 0, len(info.Counters))
	for _, r := range info.Counters {
		out = append(out, omap.New().Set("emoji", r.Reaction).Set("count", r.Count).Set("mine", r.Reaction == info.Mine))
	}
	return out, nil
}

// ── вложения ─────────────────────────────────────────────────────────────

// maxMedia — главное вложение сообщения (не клавиатура и не служебное).
func maxMedia(m *maxc.Message) maxc.Attach {
	for _, a := range m.Attaches {
		switch typ, _ := a["_type"].(string); typ {
		case "", "INLINE_KEYBOARD", "CONTROL":
		default:
			return a
		}
	}
	return nil
}

func attachType(a maxc.Attach) string { s, _ := a["_type"].(string); return s }

func attachInt(a maxc.Attach, key string) int64 {
	switch v := a[key].(type) {
	case int64:
		return v
	case uint64:
		return int64(v)
	case int8:
		return int64(v)
	case int16:
		return int64(v)
	case int32:
		return int64(v)
	case uint8:
		return int64(v)
	case uint16:
		return int64(v)
	case uint32:
		return int64(v)
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

func attachStr(a maxc.Attach, key string) string { s, _ := a[key].(string); return s }

// maxIsVoice — голосовое или кружок (их расшифровывают).
func maxIsVoice(a maxc.Attach) bool {
	return attachType(a) == "AUDIO" || (attachType(a) == "VIDEO" && attachInt(a, "videoType") == 1)
}

// maxMessage — сообщение чата по (возможно, округлённому) id.
func maxMessage(ctx context.Context, rule config.ChatRule, messageID int64) (*maxc.Conn, int64, *maxc.Message, error) {
	chatID, err := maxChatID(rule)
	if err != nil {
		return nil, 0, nil, err
	}
	c, err := maxConn(ctx)
	if err != nil {
		return nil, 0, nil, err
	}
	_, m := maxFixID(ctx, c, chatID, messageID)
	if m == nil || m.Status == "REMOVED" {
		return nil, 0, nil, &Bad{Msg: fmt.Sprintf("Сообщения %d в чате '%s' нет.", messageID, rule.Alias)}
	}
	return c, chatID, m, nil
}

// maxMediaURL — откуда скачать вложение и как назвать файл.
func maxMediaURL(ctx context.Context, c *maxc.Conn, chatID int64, m *maxc.Message, a maxc.Attach) (src, name string, err error) {
	id := strconv.FormatInt(m.ID, 10)
	switch attachType(a) {
	case "PHOTO":
		return attachStr(a, "baseUrl"), "photo-" + id + ".jpg", nil
	case "STICKER":
		return attachStr(a, "url"), "sticker-" + id + ".webp", nil
	case "FILE":
		name := attachStr(a, "name")
		if name == "" {
			name = "file-" + id
		}
		src, err = c.FileURL(ctx, chatID, m.ID, attachInt(a, "fileId"))
		return src, name, err
	case "VIDEO":
		src, err = c.VideoURL(ctx, chatID, m.ID, attachInt(a, "videoId"))
		return src, "video-" + id + ".mp4", err
	case "AUDIO":
		if src = attachStr(a, "url"); src == "" {
			src, err = c.AudioURL(ctx, chatID, m.ID, attachInt(a, "audioId"), attachStr(a, "token"))
		}
		ext := path.Ext(urlPath(src))
		if ext == "" {
			ext = ".mp3"
		}
		return src, "voice-" + id + ext, err
	}
	return "", "", &Bad{Msg: fmt.Sprintf("Во вложении %s скачивать нечего.", attachType(a))}
}

func urlPath(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Path
	}
	return raw
}

// maxDownload — вложение сообщения в dir (уже скачанное — не качаем снова).
func maxDownload(ctx context.Context, c *maxc.Conn, chatID int64, m *maxc.Message, dir string, limit int64) (*tgc.Downloaded, error) {
	a := maxMedia(m)
	if a == nil {
		return nil, &Bad{Msg: fmt.Sprintf("В сообщении %d нет вложения.", m.ID)}
	}
	got := &tgc.Downloaded{}
	if d := attachInt(a, "duration"); d > 0 {
		sec := float64(d) / 1000
		got.Duration = &sec
	}
	if size := attachInt(a, "size"); limit > 0 && size > limit {
		return nil, &Bad{Msg: fmt.Sprintf("Файл %d МБ больше предела скачивания (%d МБ).", size>>20, limit>>20)}
	}
	src, name, err := maxMediaURL(ctx, c, chatID, m, a)
	if err != nil {
		return nil, err
	}
	if src == "" {
		return nil, &Bad{Msg: "MAX не дал адреса для скачивания этого вложения."}
	}
	name = safeName(name)
	got.Name, got.Path = name, filepath.Join(dir, strconv.FormatInt(m.ID, 10)+"-"+name)
	if st, err := os.Stat(got.Path); err == nil && st.Size() > 0 {
		got.Size, got.Cached = st.Size(), true
	} else {
		n, err := maxc.Download(ctx, src, got.Path, limit)
		if err != nil {
			return nil, err
		}
		got.Size = n
	}
	if head, err := readHead(got.Path, 512); err == nil {
		got.MIME = http.DetectContentType(head)
	}
	return got, nil
}

func readHead(p string, n int) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	k, _ := f.Read(buf)
	return buf[:k], nil
}

// safeName — имя файла без путей и запрещённых в Windows символов.
func safeName(name string) string {
	// не filepath.Base: под Windows «a:» в имени он примет за диск
	name = strings.ReplaceAll(name, "\\", "/")
	name = name[strings.LastIndex(name, "/")+1:]
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(`<>:"/\|?*`, r) || r < 32 {
			return '_'
		}
		return r
	}, name)
}

func maxDownloadFile(ctx context.Context, s *config.Settings, rule config.ChatRule, messageID int64) (*omap.Map, error) {
	c, chatID, m, err := maxMessage(ctx, rule, messageID)
	if err != nil {
		return nil, err
	}
	got, err := maxDownload(ctx, c, chatID, m, filepath.Join(s.DownloadsDir(), rule.Alias), int64(s.MaxDownloadMB)*1024*1024)
	if err != nil {
		return nil, err
	}
	logEvent(s, "download", "chat", rule.Alias, "message_id", m.ID, "name", got.Name, "size", got.Size)
	return omap.New().Set("chat", s.Ref(rule.Alias)).Set("message_id", m.ID).Set("path", got.Path).
		Set("name", got.Name).Set("mime_type", got.MIME).Set("size", got.Size).Set("duration", durVal(got.Duration)).
		Set("cached", got.Cached), nil
}

func maxViewMedia(ctx context.Context, s *config.Settings, rule config.ChatRule, messageID int64) (*omap.Map, *tgc.Image, error) {
	c, chatID, m, err := maxMessage(ctx, rule, messageID)
	if err != nil {
		return nil, nil, err
	}
	a := maxMedia(m)
	kind := "photo"
	src := ""
	switch attachType(a) {
	case "PHOTO":
		src = attachStr(a, "baseUrl")
	case "STICKER":
		kind, src = "sticker", attachStr(a, "url")
	case "VIDEO":
		kind, src = "preview_frame", attachStr(a, "thumbnail")
	default:
		return nil, nil, &Bad{Msg: fmt.Sprintf("В сообщении %d нет картинки (вложение: %s).", m.ID, attachType(a))}
	}
	if src == "" {
		return nil, nil, &Bad{Msg: "У картинки нет адреса для скачивания."}
	}
	dir := filepath.Join(s.DownloadsDir(), rule.Alias)
	p := filepath.Join(dir, fmt.Sprintf("%d-%s.img", m.ID, kind))
	if _, err := os.Stat(p); err != nil {
		if _, err := maxc.Download(ctx, src, p, 20<<20); err != nil {
			return nil, nil, err
		}
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, nil, err
	}
	format := strings.TrimPrefix(http.DetectContentType(data), "image/")
	if !slices.Contains([]string{"jpeg", "png", "gif", "webp"}, format) {
		return nil, nil, &Bad{Msg: "Вложение — не картинка, которую можно показать (" + format + "); скачай его через tg_download_file."}
	}
	_ = c
	_ = chatID
	img := &tgc.Image{Data: data, Format: format, Kind: kind, Caption: m.Text}
	logEvent(s, "view_media", "chat", rule.Alias, "message_id", m.ID, "kind", kind)
	meta := omap.New().Set("chat", s.Ref(rule.Alias)).Set("message_id", m.ID).Set("kind", kind).
		Set("caption", m.Text).Set("bytes", len(data))
	return meta, img, nil
}

// maxTranscribe — расшифровка голосового: своя (GigaAM), иначе — сервера MAX.
func maxTranscribe(ctx context.Context, s *config.Settings, rule config.ChatRule, c *maxc.Conn, chatID int64,
	m *maxc.Message, engine string) (tgc.Transcript, *float64, error) {
	a := maxMedia(m)
	if a == nil || !maxIsVoice(a) {
		return tgc.Transcript{}, nil, &Bad{Msg: fmt.Sprintf("Сообщение %d — не голосовое и не кружок.", m.ID)}
	}
	var dur *float64
	if d := attachInt(a, "duration"); d > 0 {
		sec := float64(d) / 1000
		dur = &sec
	}
	cache := tgc.TranscriptCache{Path: s.TranscriptsPath()}
	key := fmt.Sprintf("%s:%d", rule.Alias, m.ID)
	local := localSTT(s, rule)
	if engine == "gigaam" {
		local.Local = forcedRussian
	}
	if engine == "max" || engine == tgc.EngineTelegram {
		local.Local = nil
	}
	if cached, ok := cache.Get(key); ok && (engine == "" || engine == "auto" || strings.HasPrefix(cached.Engine, engine)) {
		return cached, dur, nil
	}
	var tried tgc.Transcript
	if local.Local != nil {
		file, err := maxDownload(ctx, c, chatID, m, local.Dir, local.MaxBytes)
		if err != nil {
			tried.LocalError = "голосовое не скачалось: " + err.Error()
		} else if got, ok := tgc.LocalFile(ctx, file.Path, local); ok {
			cache.Put(key, got)
			return got, dur, nil
		} else {
			tried = got
		}
	}
	if attachType(a) != "AUDIO" {
		return tried, dur, &Bad{Msg: "Кружок MAX сам не расшифровывает, а своя расшифровка не вышла: " + tried.LocalError}
	}
	srv, err := c.Transcribe(ctx, chatID, m.ID, attachInt(a, "audioId"), 90*time.Second)
	if err != nil {
		return tried, dur, err
	}
	got := tgc.Transcript{Status: "pending", Engine: "max", Language: tried.Language, LocalError: tried.LocalError}
	switch srv.Status {
	case "SUCCESS":
		got.Status, got.Text = "done", srv.Text
		cache.Put(key, got)
	case "FAILED", "NOT_SUPPORTED":
		return got, dur, &Bad{Msg: "MAX не смог расшифровать (" + srv.Status + "); скачай голосовое через tg_download_file."}
	default:
		got.Partial = srv.Text
	}
	return got, dur, nil
}

func maxTranscribeMessage(ctx context.Context, s *config.Settings, rule config.ChatRule, messageID int64, engine string) (*omap.Map, error) {
	c, chatID, m, err := maxMessage(ctx, rule, messageID)
	if err != nil {
		return nil, err
	}
	got, dur, err := maxTranscribe(ctx, s, rule, c, chatID, m, engine)
	if err != nil {
		return nil, err
	}
	logEvent(s, "transcribe", "chat", rule.Alias, "message_id", m.ID, "status", got.Status, "engine", got.Engine)
	out := omap.New().Set("chat", s.Ref(rule.Alias)).Set("message_id", m.ID).Set("duration", durVal(dur))
	tgc.ApplyTranscript(out, got)
	return out, nil
}

// maxAttachTranscripts — голосовые в выдаче чтения приходят с расшифровкой
// (из кэша; новые — не больше TranscribePerCall за вызов, как в Telegram).
func maxAttachTranscripts(ctx context.Context, s *config.Settings, rule config.ChatRule, c *maxc.Conn, chatID int64,
	msgs []maxc.Message, out []*omap.Map) {
	budget := s.TranscribePerCall
	cache := tgc.TranscriptCache{Path: s.TranscriptsPath()}
	for i := range msgs {
		a := maxMedia(&msgs[i])
		if a == nil || !maxIsVoice(a) {
			continue
		}
		key := fmt.Sprintf("%s:%d", rule.Alias, msgs[i].ID)
		if cached, ok := cache.Get(key); ok {
			tgc.ApplyTranscript(out[i], cached)
			continue
		}
		if budget <= 0 {
			out[i].Set("transcript_status", "not_requested").Set("transcript", nil)
			continue
		}
		budget--
		got, _, err := maxTranscribe(ctx, s, rule, c, chatID, &msgs[i], "")
		if err != nil {
			out[i].Set("transcript_status", "error").Set("transcript", nil).Set("transcript_error", err.Error())
			continue
		}
		tgc.ApplyTranscript(out[i], got)
	}
}

// ── отправка файлов ──────────────────────────────────────────────────────

// maxUpload — файлы черновика во вложения MAX: картинки — фото (или файлом,
// если просили без сжатия), .ogg/.opus — голосовым, видео — видео, прочее —
// файлом.
func maxUpload(ctx context.Context, c *maxc.Conn, files []outbox.FileSnap) ([]maxc.Attach, error) {
	maxRT.Lock()
	ua := maxRT.ua
	maxRT.Unlock()
	var out []maxc.Attach
	for _, f := range files {
		var (
			a   maxc.Attach
			err error
		)
		ext := strings.ToLower(filepath.Ext(f.Path))
		switch {
		case f.Kind == outbox.KindPhoto:
			a, err = c.UploadPhoto(ctx, f.Path)
		case ext == ".ogg" || ext == ".oga" || ext == ".opus":
			a, err = c.UploadVoice(ctx, f.Path, time.Duration(tgc.VoiceDuration(f.Path))*time.Second, ua)
		case ext == ".mp4" || ext == ".mov" || ext == ".webm" || ext == ".m4v":
			a, err = c.UploadVideo(ctx, f.Path)
		default:
			a, err = c.UploadFile(ctx, f.Path)
		}
		if err != nil {
			return nil, fmt.Errorf("%s не загрузился в MAX: %w", f.Name, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// maxMediaInfo — что агенту знать о вложении в выдаче чтения.
func maxMediaInfo(out *omap.Map, a maxc.Attach) {
	switch attachType(a) {
	case "FILE":
		out.Set("file", omap.New().Set("name", attachStr(a, "name")).Set("size", attachInt(a, "size")))
	case "AUDIO", "VIDEO":
		if d := attachInt(a, "duration"); d > 0 {
			out.Set("duration", float64(d)/1000)
		}
	case "SHARE":
		out.Set("link_preview", omap.New().Set("url", attachStr(a, "url")).Set("title", attachStr(a, "title")))
	case "CONTACT":
		out.Set("contact", omap.New().Set("id", attachInt(a, "contactId")).Set("name", attachStr(a, "name")))
	}
}

// maxFolderRules — правила папки словами.
func maxFolderRules(f maxc.Folder) []string {
	words := map[int]string{maxc.FilterUnread: "непрочитанные", maxc.FilterRead: "прочитанные",
		maxc.FilterChannel: "каналы", maxc.FilterChat: "группы", maxc.FilterDialog: "личные",
		maxc.FilterOwner: "где владелец", maxc.FilterAdmin: "где админ", maxc.FilterMuted: "без звука",
		maxc.FilterContact: "контакты", maxc.FilterNotContact: "не контакты", maxc.FilterBot: "боты",
		maxc.FilterNotMuted: "со звуком", maxc.FilterMarkedUnread: "отмеченные непрочитанными",
		maxc.FilterOrg: "организации"}
	var out []string
	for _, x := range f.Filters {
		if w, ok := words[x]; ok {
			out = append(out, w)
		}
	}
	return out
}
