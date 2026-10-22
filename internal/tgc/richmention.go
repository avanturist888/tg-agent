package tgc

// Упоминания по id в rich-сообщении. Разметку [Имя](tg://user?id=…) сервер
// превращает в упоминание, только если знает человека и без access_hash
// (сам аккаунт, контакты); участника той же группы он молча выбрасывает —
// уходит просто имя, без уведомления (проверено 21.10.2026). Поэтому текст
// с такими ссылками разбирает сам сервер — в черновик Избранного, ссылки
// заменены заглушкой, — а уходят уже готовые блоки: заглушки становятся
// TextMentionName, access_hash людей — в users. Вид сообщения тот же, что
// у обычной отправки markdown.

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
)

var mentionLink = regexp.MustCompile(`\]\(\s*tg://user\?id=(\d+)\s*\)`)

const mentionStub = "https://tg-agent.invalid/mention/"

// stubMentions — ссылки на людей, чей access_hash известен, → заглушки.
// users — id → access_hash заменённых.
func stubMentions(text string, hash func(int64) (int64, bool)) (string, map[int64]int64) {
	users := map[int64]int64{}
	out := mentionLink.ReplaceAllStringFunc(text, func(m string) string {
		id, _ := strconv.ParseInt(mentionLink.FindStringSubmatch(m)[1], 10, 64)
		h, ok := hash(id)
		if !ok {
			return m // не знаем — пусть сервер попробует сам
		}
		users[id] = h
		return "](" + mentionStub + strconv.FormatInt(id, 10) + ")"
	})
	return out, users
}

var richTextType = reflect.TypeOf((*tg.RichTextClass)(nil)).Elem()

// unstub — в блоках ссылки-заглушки → упоминания по id; заглушка, попавшая
// в текст (например, внутри `кода`), — обратно в tg://user?id=. found —
// кого упомянули.
func unstub(v reflect.Value, found map[int64]bool) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			unstub(v.Elem(), found)
		}
	case reflect.Interface:
		if v.IsNil() {
			return
		}
		if u, ok := v.Interface().(*tg.TextURL); ok && v.Type() == richTextType && v.CanSet() {
			if id, err := strconv.ParseInt(strings.TrimPrefix(u.URL, mentionStub), 10, 64); err == nil && strings.HasPrefix(u.URL, mentionStub) {
				v.Set(reflect.ValueOf(tg.RichTextClass(&tg.TextMentionName{Text: u.Text, UserID: id})))
				found[id] = true
				return
			}
		}
		unstub(v.Elem(), found)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				unstub(v.Field(i), found)
			}
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			unstub(v.Index(i), found)
		}
	case reflect.String:
		if v.CanSet() && strings.Contains(v.String(), mentionStub) {
			v.SetString(strings.ReplaceAll(v.String(), mentionStub, "tg://user?id="))
		}
	}
}

// richInput — rich message из markdown для отправки или правки. Без ссылок
// на знакомых людей — markdown как есть (его разбирает сервер при отправке).
func (c *Conn) richInput(ctx context.Context, text string) (tg.InputRichMessageClass, error) {
	md := &tg.InputRichMessageMarkdown{Markdown: text}
	if c == nil || c.Peers == nil || !mentionLink.MatchString(text) {
		return md, nil
	}
	stubbed, users := stubMentions(text, c.Peers.user)
	if len(users) == 0 {
		return md, nil
	}
	blocks, err := c.parseRich(ctx, stubbed)
	if err != nil {
		return nil, err
	}
	found := map[int64]bool{}
	unstub(reflect.ValueOf(blocks), found)
	in := &tg.InputRichMessage{Blocks: blocks}
	var list []tg.InputUserClass
	for id := range found {
		list = append(list, &tg.InputUser{UserID: id, AccessHash: users[id]})
	}
	if len(list) > 0 {
		in.SetUsers(list)
	}
	return in, nil
}

// draftMu — разбор занимает черновик Избранного: по одному.
var draftMu sync.Mutex

// parseRich — markdown → блоки руками сервера: через черновик Избранного.
// Прежний черновик там (если был) возвращается на место.
func (c *Conn) parseRich(ctx context.Context, markdown string) ([]tg.PageBlockClass, error) {
	draftMu.Lock()
	defer draftMu.Unlock()
	prev, err := c.selfDraft(ctx)
	if err != nil {
		return nil, err
	}
	req := &tg.MessagesSaveDraftRequest{Peer: &tg.InputPeerSelf{}, NoWebpage: true}
	req.SetRichMessage(&tg.InputRichMessageMarkdown{Markdown: markdown})
	if _, err := c.API.MessagesSaveDraft(ctx, req); err != nil {
		return nil, err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_, _ = c.API.MessagesSaveDraft(rctx, restoreDraft(prev))
	}()
	d, err := c.selfDraft(ctx)
	if err != nil {
		return nil, err
	}
	dm, ok := d.(*tg.DraftMessage)
	if !ok {
		return nil, errors.New("Telegram не сохранил черновик для разбора разметки")
	}
	rich, ok := dm.GetRichMessage()
	if !ok || len(rich.Blocks) == 0 {
		return nil, errors.New("Telegram не разобрал разметку в черновике")
	}
	return rich.Blocks, nil
}

func (c *Conn) selfDraft(ctx context.Context) (tg.DraftMessageClass, error) {
	res, err := c.API.MessagesGetPeerDialogs(ctx, []tg.InputDialogPeerClass{&tg.InputDialogPeer{Peer: &tg.InputPeerSelf{}}})
	if err != nil {
		return nil, err
	}
	for _, d := range res.Dialogs {
		if dd, ok := d.(*tg.Dialog); ok {
			draft, _ := dd.GetDraft()
			return draft, nil
		}
	}
	return nil, errors.New("Telegram не вернул диалог Избранного")
}

// restoreDraft — запрос, возвращающий черновик prev (пустой — очистка).
func restoreDraft(prev tg.DraftMessageClass) *tg.MessagesSaveDraftRequest {
	req := &tg.MessagesSaveDraftRequest{Peer: &tg.InputPeerSelf{}}
	p, ok := prev.(*tg.DraftMessage)
	if !ok {
		return req
	}
	req.NoWebpage, req.InvertMedia, req.Message = p.NoWebpage, p.InvertMedia, p.Message
	if v, ok := p.GetEntities(); ok {
		req.SetEntities(v)
	}
	if v, ok := p.GetReplyTo(); ok {
		req.SetReplyTo(v)
	}
	if v, ok := p.GetMedia(); ok {
		req.SetMedia(v)
	}
	if v, ok := p.GetEffect(); ok {
		req.SetEffect(v)
	}
	if v, ok := p.GetRichMessage(); ok {
		req.SetRichMessage(&tg.InputRichMessage{Blocks: v.Blocks})
	}
	return req
}
