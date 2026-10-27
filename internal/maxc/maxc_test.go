package maxc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/klauspost/compress/zstd"
)

func TestFrameRoundTrip(t *testing.T) {
	data, err := encodeFrame(CmdRequest, 7, OpMsgSend, map[string]any{"chatId": int64(-5), "text": "привет"})
	if err != nil {
		t.Fatal(err)
	}
	if data[0] != protoVersion || binary.BigEndian.Uint16(data[4:]) != OpMsgSend ||
		int(binary.BigEndian.Uint32(data[6:])) != len(data)-headerSize {
		t.Fatalf("заголовок % x", data[:headerSize])
	}
	frames, err := decodeFrames(append(data, data...)) // два кадра в одном сообщении
	if err != nil || len(frames) != 2 || frames[1].Seq != 7 {
		t.Fatalf("%v %+v", err, frames)
	}
	var v struct {
		ChatID int64  `json:"chatId"`
		Text   string `json:"text"`
	}
	if err := frames[0].Decode(&v); err != nil || v.ChatID != -5 || v.Text != "привет" {
		t.Fatalf("%v %+v", err, v)
	}
	if _, err := decodeFrames(data[:len(data)-1]); err == nil {
		t.Fatal("обрезанный кадр принят")
	}
}

// lz4Seq — последовательность блока LZ4: литералы и (offset > 0) совпадение.
func lz4Seq(lit []byte, offset, match int) []byte {
	ext := func(out []byte, n int) []byte {
		for ; n >= 255; n -= 255 {
			out = append(out, 255)
		}
		return append(out, byte(n))
	}
	litN, matchN := min(len(lit), 15), 0
	if offset > 0 {
		matchN = min(match-4, 15)
	}
	out := []byte{byte(litN<<4 | matchN)}
	if litN == 15 {
		out = ext(out, len(lit)-15)
	}
	out = append(out, lit...)
	if offset == 0 {
		return out
	}
	out = append(out, byte(offset), byte(offset>>8))
	if matchN == 15 {
		out = ext(out, match-4-15)
	}
	return out
}

// packed — кадр сервера со сжатым телом.
func packed(t *testing.T, flags byte, body []byte) []byte {
	t.Helper()
	out := make([]byte, headerSize)
	out[0], out[1] = protoVersion, CmdResponse
	binary.BigEndian.PutUint32(out[6:], uint32(flags)<<24|uint32(len(body)))
	return append(out, body...)
}

func TestCompressedFrames(t *testing.T) {
	raw, _ := marshal(map[string]any{"text": strings.Repeat("ab", 50)})
	// LZ4 вручную: литералы до первого "ab", затем совпадение с самоперекрытием
	i := bytes.Index(raw, []byte("abab"))
	lit, rest := raw[:i+2], raw[i+2:]
	matchLen := 98 // остаток "ab…" после первых двух байт
	tail := rest[matchLen:]
	blk := lz4Seq(lit, 2, matchLen)
	blk = append(blk, lz4Seq(tail, 0, 0)...) // последний кусок — одни литералы

	enc, _ := zstd.NewWriter(nil)
	for name, data := range map[string][]byte{
		"lz4":  packed(t, 0x80, blk), // 0x80+ — тоже LZ4 (PyMax 2.5.0)
		"zstd": packed(t, 0xFF, enc.EncodeAll(raw, nil)),
	} {
		frames, err := decodeFrames(data)
		if err != nil || len(frames) != 1 || !bytes.Equal(frames[0].Payload, raw) {
			t.Fatalf("%s: %v %v", name, err, frames)
		}
	}
	if _, err := unlz4([]byte{0x10, 'a', 5, 0}, 100); err == nil {
		t.Fatal("смещение за началом принято")
	}
}

// ext — обёртка msgpack: ext8 с типом typ вокруг data.
func ext(typ byte, data []byte) []byte {
	return append([]byte{0xc7, byte(len(data)), typ}, data...)
}

func TestWrappedValues(t *testing.T) {
	inner, _ := marshal(int64(1792666937000))
	nested, _ := marshal(map[string]any{"id": 5})
	// {"time": ext1(int), "chat": ext1({"id":5}), "list": [ext1(int), fixext ext5], "s": "x"}
	var b []byte
	b = append(b, 0x84)
	b = append(b, 0xa4, 't', 'i', 'm', 'e')
	b = append(b, ext(1, inner)...)
	b = append(b, 0xa4, 'c', 'h', 'a', 't')
	b = append(b, ext(1, nested)...)
	b = append(b, 0xa4, 'l', 'i', 's', 't', 0x92)
	b = append(b, ext(1, inner)...)
	b = append(b, 0xd4, 5, 0x07) // чужое расширение — nil
	b = append(b, 0xa1, 's', 0xa1, 'x')
	frames, err := decodeFrames(packed(t, 0, b))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Time int64 `json:"time"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		List []any  `json:"list"`
		S    string `json:"s"`
	}
	if err := frames[0].Decode(&v); err != nil || v.Time != 1792666937000 || v.Chat.ID != 5 || len(v.List) != 2 || v.List[1] != nil || v.S != "x" {
		t.Fatalf("%v %+v", err, v)
	}
	if _, err := unwrapExt([]byte{0x92, 0xc7, 9, 1, 0x01}); err == nil {
		t.Fatal("обрезанное расширение принято")
	}
}

func TestLooseKeys(t *testing.T) {
	raw, _ := marshal(map[int64]any{42: map[int64]int64{7: 1}})
	var v any
	if err := unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	m, ok := v.(map[string]any)
	if !ok || m["42"] == nil {
		t.Fatalf("%#v", v)
	}
}

// ── поддельный сервер ────────────────────────────────────────────────────

type fakeServer struct {
	t        *testing.T
	mu       sync.Mutex
	ops      []uint16
	ua       map[string]any
	token    string   // какой токен принимать
	password string   // "" — без облачного пароля
	polls    int      // сколько опросов QR до подтверждения
	pushes   int      // сколько событий слать после LOGIN
	extra    [][]byte // ещё события после LOGIN
	drops    int      // столько раз порвать связь сразу после LOGIN
	logins   int
	sent     []map[string]any // тела MSG_SEND
	hang     bool             // не отвечать на запросы
}

func (s *fakeServer) start() string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != DefaultOrigin {
			http.Error(w, "origin", http.StatusForbidden)
			return
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ctx := r.Context()
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			frames, err := decodeFrames(data)
			if err != nil {
				s.t.Error(err)
				return
			}
			for _, f := range frames {
				outs, drop := s.answer(f)
				for _, out := range outs {
					if err := ws.Write(ctx, websocket.MessageBinary, out); err != nil {
						return
					}
				}
				if drop {
					ws.CloseNow()
					return
				}
			}
		}
	}))
	s.t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func (s *fakeServer) answer(f Frame) (out [][]byte, drop bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, f.Opcode)
	if s.hang {
		return nil, false
	}
	return s.reply(f), f.Opcode == OpLogin && s.drops >= s.logins && s.logins > 0
}

func (s *fakeServer) reply(f Frame) [][]byte {
	var req map[string]any
	_ = f.Decode(&req)
	reply := func(cmd byte, v any) [][]byte {
		b, err := encodeFrame(cmd, f.Seq, f.Opcode, v)
		if err != nil {
			s.t.Fatal(err)
		}
		return [][]byte{b}
	}
	fail := func(code string) [][]byte {
		return reply(CmdError, map[string]any{"error": code, "message": code, "localizedMessage": "нельзя"})
	}
	switch f.Opcode {
	case OpSessionInit:
		s.ua, _ = req["userAgent"].(map[string]any)
		if req["deviceId"] == "" {
			return fail("proto.payload")
		}
		return reply(CmdResponse, map[string]any{"location": "RU"})
	case OpGetQR:
		return reply(CmdResponse, map[string]any{"trackId": "T1", "qrLink": "https://max.ru/:auth/T1",
			"expiresAt": time.Now().Add(time.Minute).UnixMilli(), "pollingInterval": 1, "ttl": 120})
	case OpQRStatus:
		s.polls--
		st := map[string]any{"expiresAt": time.Now().Add(time.Minute).UnixMilli()}
		if s.polls <= 0 {
			st["loginAvailable"] = true
		}
		return reply(CmdResponse, map[string]any{"status": st})
	case OpLoginByQR:
		if s.password != "" {
			return reply(CmdResponse, map[string]any{"tokenAttrs": map[string]any{},
				"passwordChallenge": map[string]any{"trackId": "P1", "hint": "кот"}})
		}
		return reply(CmdResponse, map[string]any{"tokenAttrs": map[string]any{"LOGIN": map[string]any{"token": s.token}}})
	case OpCheckPassword:
		if req["password"] != s.password || req["trackId"] != "P1" {
			return fail("password.invalid")
		}
		return reply(CmdResponse, map[string]any{"tokenAttrs": map[string]any{"LOGIN": map[string]any{"token": s.token}}})
	case OpLogin:
		if req["token"] != s.token {
			return fail("FAIL_LOGIN_TOKEN")
		}
		s.logins++
		out := reply(CmdResponse, map[string]any{
			"profile": map[string]any{"contact": map[string]any{"id": 100, "names": []any{map[string]any{"name": "Владелец"}}}},
			"chats": []any{map[string]any{"id": 0, "type": "DIALOG", "participants": map[int64]int64{100: 1}},
				map[string]any{"id": -9, "type": "CHAT", "title": "Группа"}},
		})
		for i := range s.pushes { // события — сразу за ответом
			b, _ := encodeFrame(CmdRequest, uint16(500+i), OpNotifMessage, map[string]any{
				"chatId": -9, "message": map[string]any{"id": i + 1, "text": "x", "sender": 5}})
			out = append(out, b)
		}
		return append(out, s.extra...)
	case OpChatHistory:
		if fmt.Sprint(req["chatId"], req["backward"], req["getMessages"]) != "-9 2 true" {
			return fail("proto.payload")
		}
		return reply(CmdResponse, map[string]any{"messages": []any{
			map[string]any{"id": 1, "time": 10, "text": "раз", "sender": 5, "type": "USER"},
			map[string]any{"id": 2, "time": 20, "text": "", "sender": 100, "type": "USER",
				"link": map[string]any{"type": "FORWARD", "messageId": "77", "chatId": 5,
					"message": map[string]any{"id": 77, "text": "исходное"}}},
		}})
	case OpMsgSend:
		s.sent = append(s.sent, req)
		m := req["message"].(map[string]any)
		return reply(CmdResponse, map[string]any{"chatId": req["chatId"], "message": map[string]any{
			"id": 900 + len(s.sent), "cid": m["cid"], "text": m["text"], "sender": 100, "type": "USER", "time": 1}})
	case OpMsgEdit:
		return reply(CmdResponse, map[string]any{"message": map[string]any{
			"id": req["messageId"], "text": req["text"], "status": "EDITED", "updateTime": 2}})
	case OpMsgDelete:
		if req["forMe"] != false {
			return fail("proto.payload")
		}
		return reply(CmdResponse, map[string]any{"chatId": req["chatId"], "messageIds": req["messageIds"]})
	}
	return fail("unknown.opcode")
}

func TestLoginQRWithPassword(t *testing.T) {
	fs := &fakeServer{t: t, token: "TOK", password: "secret", polls: 2}
	url := fs.start()
	ctx := context.Background()
	c, err := Dial(ctx, Options{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ua := WebUserAgent("26.10.7")
	if err := c.Handshake(ctx, ua, NewDeviceID()); err != nil {
		t.Fatal(err)
	}
	if fs.ua["appVersion"] != "26.10.7" || fs.ua["deviceType"] != "WEB" {
		t.Fatalf("user-agent на сервере: %v", fs.ua)
	}
	var shown QR
	asked := 0
	_, err = c.LoginQR(ctx, func(q QR) error { shown = q; return nil },
		func(hint string, prev error) (string, error) { asked++; return "wrong", nil })
	var e *Error
	if !errors.As(err, &e) || e.Code != "password.invalid" || e.Localized != "нельзя" || asked != passwordAttempts {
		t.Fatalf("неверный пароль: %v, спрошен %d раз", err, asked)
	}
	if shown.Link != "https://max.ru/:auth/T1" || shown.ExpiresAt().Before(time.Now()) {
		t.Fatalf("QR: %+v", shown)
	}
	// ошибся раз — спрашивает снова, с причиной и подсказкой
	var log []string
	tok, err := c.LoginQR(ctx, func(QR) error { return nil }, func(hint string, prev error) (string, error) {
		log = append(log, hint+"|"+map[bool]string{true: "повтор", false: "первый"}[prev != nil])
		if prev == nil {
			return "wrong", nil
		}
		return "secret", nil
	})
	if err != nil || tok != "TOK" || strings.Join(log, ",") != "кот|первый,кот|повтор" {
		t.Fatalf("вход: %q %v, вопросы %q", tok, err, log)
	}
	if _, err := c.LoginQR(ctx, func(QR) error { return nil }, nil); !errors.Is(err, ErrPasswordRequired) {
		t.Fatalf("без спрашивающего пароль: %v", err)
	}
}

func TestOpenAndEvents(t *testing.T) {
	fs := &fakeServer{t: t, token: "TOK", pushes: 50}
	url := fs.start()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "max", "session.json")
	sess := &Session{Token: "TOK", DeviceID: NewDeviceID(), UserAgent: WebUserAgent(""), Source: "qr"}
	if err := sess.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSession(path)
	if err != nil || loaded.Token != "TOK" {
		t.Fatalf("%v %+v", err, loaded)
	}

	var mu sync.Mutex
	var ids []int64
	all := make(chan struct{})
	c, r, err := Open(ctx, loaded, Options{URL: url, OnEvent: func(f Frame) {
		var ev struct {
			Message Message `json:"message"`
		}
		if f.Opcode != OpNotifMessage || f.Decode(&ev) != nil {
			return
		}
		mu.Lock()
		ids = append(ids, ev.Message.ID)
		if len(ids) == 50 {
			close(all)
		}
		mu.Unlock()
		time.Sleep(time.Millisecond) // медленный обработчик не путает порядок
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if r.Profile.Contact.ID != 100 || r.Profile.Contact.DisplayName() != "Владелец" || len(r.Chats) != 2 || r.Chats[1].Title != "Группа" {
		t.Fatalf("вход: %+v", r)
	}
	select {
	case <-all:
	case <-time.After(5 * time.Second):
		t.Fatalf("дошло событий: %d", len(ids))
	}
	for i, id := range ids {
		if id != int64(i+1) {
			t.Fatalf("порядок событий: %v", ids)
		}
	}
	// запросы параллельно — ответы не путаются
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if err := c.Handshake(ctx, sess.UserAgent, sess.DeviceID); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	// отозванный токен
	_, _, err = Open(ctx, &Session{Token: "OLD", DeviceID: "d", UserAgent: WebUserAgent("")}, Options{URL: url})
	if !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("отозванный токен: %v", err)
	}
	c.Close()
	if err := c.Invoke(ctx, OpPing, nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("после закрытия: %v", err)
	}
}

func TestMessagesAndEvents(t *testing.T) {
	fs := &fakeServer{t: t, token: "TOK"}
	ev := func(op uint16, v any) []byte {
		b, err := encodeFrame(CmdRequest, 0, op, v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	fs.extra = [][]byte{
		ev(OpNotifMessage, map[string]any{"chatId": -9, "unread": 3, "prevMessageId": "41",
			"message": map[string]any{"id": 42, "text": "новое", "sender": 5, "elements": []any{map[string]any{"type": "STRONG", "length": 5}}}}),
		ev(OpNotifMessage, map[string]any{"chatId": -9, "message": map[string]any{"id": 42, "text": "правка", "status": "EDITED"}}),
		ev(OpNotifMessage, map[string]any{"chatId": -9, "message": map[string]any{"id": 42, "status": "REMOVED"}}),
		ev(OpNotifMsgDelete, map[string]any{"chatId": -9, "messageIds": []any{40, 41}}),
		ev(OpNotifMark, map[string]any{"chatId": -9, "userId": 5, "mark": 1234, "unread": 0, "setAsUnread": false}),
		ev(999, map[string]any{"x": 1}),
	}
	url := fs.start()
	got := make(chan Event, 10)
	cl := &Client{Session: &Session{Token: "TOK", DeviceID: "d", UserAgent: WebUserAgent("")},
		Options: Options{URL: url}, OnEvent: func(e Event) { got <- e }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cl.Run(ctx)

	var evs []Event
	for range len(fs.extra) {
		select {
		case e := <-got:
			evs = append(evs, e)
		case <-time.After(5 * time.Second):
			t.Fatalf("событий %d", len(evs))
		}
	}
	if e := evs[0]; e.Kind != EventNew || e.ChatID != -9 || e.Message.ID != 42 || e.Message.Text != "новое" ||
		e.Unread != 3 || len(e.Message.Elements) != 1 || e.Message.Elements[0].Type != "STRONG" {
		t.Fatalf("новое: %+v %+v", e, e.Message)
	}
	if e := evs[1]; e.Kind != EventEdit || e.Message.Text != "правка" {
		t.Fatalf("правка: %+v", e)
	}
	if e := evs[2]; e.Kind != EventDelete || len(e.MessageIDs) != 1 || e.MessageIDs[0] != 42 {
		t.Fatalf("удаление статусом: %+v", e)
	}
	if e := evs[3]; e.Kind != EventDelete || len(e.MessageIDs) != 2 || e.MessageIDs[1] != 41 {
		t.Fatalf("удаление опкодом: %+v", e)
	}
	if e := evs[4]; e.Kind != EventRead || e.Read.UserID != 5 || e.Read.Mark != 1234 {
		t.Fatalf("прочтение: %+v %+v", e, e.Read)
	}
	if e := evs[5]; e.Kind != EventOther || e.Frame.Opcode != 999 {
		t.Fatalf("незнакомое: %+v", e)
	}

	c, err := cl.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := c.History(ctx, -9, 0, 2)
	if err != nil || len(hist) != 2 || hist[0].Text != "раз" || hist[1].Link == nil ||
		hist[1].Link.MessageID != 77 || hist[1].Link.Message.Text != "исходное" {
		t.Fatalf("история: %v %+v", err, hist)
	}
	m1, err := c.Send(ctx, SavedChat, "проба", SendOptions{ReplyTo: 2})
	if err != nil {
		t.Fatal(err)
	}
	m2, err := c.Send(ctx, SavedChat, "проба 2", SendOptions{Silent: true})
	if err != nil || m1.ID != 901 || m2.CID <= m1.CID || m1.Text != "проба" {
		t.Fatalf("отправка: %v %+v %+v", err, m1, m2)
	}
	fs.mu.Lock()
	first, second := fs.sent[0], fs.sent[1]
	fs.mu.Unlock()
	link, _ := first["message"].(map[string]any)["link"].(map[string]any)
	if first["notify"] != true || second["notify"] != false || link["type"] != "REPLY" || fmt.Sprint(link["messageId"]) != "2" ||
		first["message"].(map[string]any)["elements"] == nil {
		t.Fatalf("тела отправки: %v | %v", first, second)
	}
	ed, err := c.Edit(ctx, SavedChat, m1.ID, "исправлено", nil)
	if err != nil || ed.Status != "EDITED" || ed.Text != "исправлено" {
		t.Fatalf("правка: %v %+v", err, ed)
	}
	if err := c.Delete(ctx, SavedChat, []int64{m1.ID, m2.ID}, false); err != nil {
		t.Fatal(err)
	}
}

func TestClientReconnects(t *testing.T) {
	old := backoffMin
	backoffMin = 10 * time.Millisecond
	defer func() { backoffMin = old }()
	fs := &fakeServer{t: t, token: "TOK", drops: 2}
	url := fs.start()
	logins, drops := make(chan int, 10), 0
	n := 0
	cl := &Client{Session: &Session{Token: "TOK", DeviceID: "d", UserAgent: WebUserAgent("")}, Options: Options{URL: url},
		OnLogin:      func(*LoginResult) { n++; logins <- n },
		OnDisconnect: func(error) { drops++ }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cl.Run(ctx) }()
	for want := 1; want <= 3; want++ {
		select {
		case got := <-logins:
			if got != want {
				t.Fatalf("вход %d, ждали %d", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("входов %d", want-1)
		}
	}
	c, err := cl.Conn(ctx) // после двух обрывов связь есть
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.History(ctx, -9, 0, 2); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || drops != 2 {
		t.Fatalf("Run: %v, обрывов %d", err, drops)
	}
	wctx, wcancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer wcancel()
	if _, err := cl.Conn(wctx); err == nil {
		t.Fatal("после остановки есть соединение")
	}

	// токен отозвали — Run останавливается сам
	cl2 := &Client{Session: &Session{Token: "OLD", DeviceID: "d", UserAgent: WebUserAgent("")}, Options: Options{URL: url}}
	if err := cl2.Run(context.Background()); !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("отозванный токен: %v", err)
	}
}

func TestFolderMatches(t *testing.T) {
	contacts := map[int64]bool{5: true}
	dialog := Chat{ID: 1, Type: "DIALOG", Participants: Participants{100: 1, 5: 1}}
	stranger := Chat{ID: 2, Type: "DIALOG", Participants: Participants{100: 1, 7: 1}, NewMessages: 2}
	channel := Chat{ID: 3, Type: "CHANNEL"}
	group := Chat{ID: 4, Type: "CHAT", Owner: 100, NewMessages: 1}
	flags := func(c Chat) uint64 { return ChatFlags(c, 100, contacts) }
	unread := Folder{Filters: []int{FilterUnread}}
	channels := Folder{Filters: []int{FilterChannel}}
	personal := Folder{Filters: []int{FilterContact, FilterNotContact}}
	mine := Folder{Filters: []int{FilterChat, FilterOwner}, Exclude: []int64{9}, Include: []int64{3}}
	for _, tc := range []struct {
		f    Folder
		c    Chat
		want bool
	}{
		{unread, stranger, true}, {unread, dialog, false}, {unread, group, true},
		{channels, channel, true}, {channels, group, false},
		{personal, dialog, true}, {personal, stranger, true}, {personal, group, false},
		{mine, group, true}, {mine, channel, true}, {mine, dialog, false},
		{Folder{ID: AllFolder}, channel, true}, {Folder{}, channel, false},
	} {
		if got := tc.f.Matches(tc.c.ID, flags(tc.c)); got != tc.want {
			t.Errorf("папка %v, чат %d: %v, ждали %v", tc.f.Filters, tc.c.ID, got, tc.want)
		}
	}
}
