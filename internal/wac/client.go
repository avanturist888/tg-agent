// Package wac — клиент WhatsApp для tg-agent: вход как связанное устройство
// (WhatsApp Web, протокол нескольких устройств; библиотека whatsmeow),
// хранилище сообщений и действия, которые нужны шлюзу.
//
// Ключи устройства — data/wa/device.db (это полный доступ к аккаунту: никому
// не давать), сообщения — data/wa/messages.db (store.go). Соединение держит
// только служба профиля — и вход (связка устройства) идёт в ней же: второе
// соединение того же устройства WhatsApp обрывает первое (StreamReplaced), а
// история, которую телефон шлёт сразу после связки, должна попасть в
// хранилище, а не в чужой процесс.
package wac

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// DeviceName — как шлюз виден в «Связанных устройствах» на телефоне.
const DeviceName = "tg-agent"

func init() {
	store.SetOSInfo(DeviceName, [3]uint32{1, 0, 0})
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()
}

var (
	// ErrNoSession — входа нет: владелец должен связать устройство (tg wa login).
	ErrNoSession = errors.New("Входа в WhatsApp нет: владелец связывает устройство — tg --profile <профиль> wa login.")
	// ErrLoggedOut — устройство отвязали на телефоне (или WhatsApp сам).
	ErrLoggedOut = errors.New("WhatsApp отвязал устройство шлюза (выход на телефоне) — нужен новый вход.")
	// ErrReplaced — то же устройство подключилось из другого процесса.
	ErrReplaced = errors.New("WhatsApp: то же устройство подключилось из другого процесса.")
	// ErrClientOutdated — сервер отверг версию веб-клиента, которой
	// представляется whatsmeow (405): нужна свежая whatsmeow. Вход при этом цел.
	ErrClientOutdated = errors.New("WhatsApp отверг клиент шлюза как устаревший (405): нужна свежая whatsmeow.")
)

func devicePath(dir string) string { return filepath.Join(dir, "device.db") }

func openContainer(ctx context.Context, dir string) (*sqlstore.Container, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return sqlstore.New(ctx, "sqlite", "file:"+devicePath(dir)+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)",
		waLog.Noop)
}

// HasSession — есть ли связанное устройство (без подключения).
func HasSession(ctx context.Context, dir string) bool {
	if _, err := os.Stat(devicePath(dir)); err != nil {
		return false
	}
	c, err := openContainer(ctx, dir)
	if err != nil {
		return false
	}
	defer c.Close()
	d, err := c.GetFirstDevice(ctx)
	return err == nil && d.ID != nil
}

// EventKind — что случилось в чате.
type EventKind int

const (
	EventNew EventKind = iota
	EventEdit
	EventDelete
)

// Event — изменение в чате для шлюза (ленты, подписки).
type Event struct {
	Kind   EventKind
	Chat   Chat
	IDs    []int64
	FromMe bool
}

// Pair — связать устройство в этом соединении (устройства ещё нет).
type Pair struct {
	Phone    string                                 // вход кодом по номеру; пусто — QR
	OnQR     func(code string, valid time.Duration) // очередной QR
	OnCode   func(code string)                      // код из 8 знаков
	OnPaired func(phone string)                     // связано
}

// Client — соединение службы с WhatsApp. Store открывает и закрывает
// вызывающий: хранилище живёт дольше соединения.
type Client struct {
	Dir     string
	Store   *Store
	Pair    *Pair
	OnEvent func(Event)                   // новое, правка, удаление (только живые)
	OnLog   func(event string, kv ...any) // в аудит

	mu        sync.Mutex
	wa        *whatsmeow.Client
	connected bool
	lost      chan error // выход или замена: Run заканчивается
	names     map[string]string
}

func (c *Client) log(event string, kv ...any) {
	if c.OnLog != nil {
		c.OnLog(event, kv...)
	}
}

// Run — подключиться и держать соединение, пока не отменят ctx (whatsmeow
// переподключается сам). ErrNoSession — входа нет и Pair не задан;
// ErrLoggedOut — отвязали.
func (c *Client) Run(ctx context.Context) error {
	if c.Store == nil {
		return errors.New("wac: нет хранилища сообщений")
	}
	container, err := openContainer(ctx, c.Dir)
	if err != nil {
		return err
	}
	defer container.Close()
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return err
	}
	pairing := device.ID == nil
	if pairing && c.Pair == nil {
		return ErrNoSession
	}
	// предупреждения и ошибки whatsmeow — в вывод службы (data/service.log)
	wa := whatsmeow.NewClient(device, newWarnLog("whatsmeow"))
	wa.EnableAutoReconnect = true
	wa.AutomaticMessageRerequestFromPhone = true
	// присутствие («в сети») не шлём: иначе телефон перестаёт показывать
	// владельцу уведомления; прочтения — тоже (собеседники увидели бы «прочитано»)
	c.mu.Lock()
	c.wa, c.lost = wa, make(chan error, 1)
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.wa, c.connected = nil, false
		c.mu.Unlock()
	}()
	wa.AddEventHandler(func(evt any) { c.handle(ctx, evt) })
	var qr <-chan whatsmeow.QRChannelItem
	if pairing && c.Pair.Phone == "" {
		if qr, err = wa.GetQRChannel(ctx); err != nil {
			return err
		}
	}
	if err := wa.ConnectContext(ctx); err != nil {
		return err
	}
	defer wa.Disconnect()
	if pairing {
		if c.Pair.Phone != "" {
			code, err := wa.PairPhone(ctx, c.Pair.Phone, true, whatsmeow.PairClientChrome, "Chrome (Windows)")
			if err != nil {
				return fmt.Errorf("код для входа по номеру не выдан: %w", err)
			}
			c.Pair.OnCode(code)
		} else {
			go c.watchQR(qr)
		}
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-c.lost:
		if errors.Is(err, ErrLoggedOut) {
			_ = container.DeleteDevice(context.WithoutCancel(ctx), device) // ключи отозваны — только мешают новому входу
		}
		return err
	}
}

func (c *Client) watchQR(qr <-chan whatsmeow.QRChannelItem) {
	for item := range qr {
		switch item.Event {
		case "code":
			c.Pair.OnQR(item.Code, item.Timeout)
		case "success":
			return
		case "timeout":
			c.fail(errors.New("QR-коды кончились, никто не отсканировал — запусти вход снова"))
			return
		default:
			if item.Error != nil {
				c.fail(fmt.Errorf("вход не удался: %w", item.Error))
			} else {
				c.fail(fmt.Errorf("вход не удался: %s", item.Event))
			}
			return
		}
	}
}

// Logout — отвязать устройство (перед новым входом). whatsmeow после своего
// выхода событий не шлёт — Run заканчиваем сами, как при LoggedOut.
func (c *Client) Logout(ctx context.Context) error {
	wa, err := c.WA(ctx)
	if err != nil {
		return err
	}
	if err := wa.Logout(ctx); err != nil {
		return err
	}
	c.fail(ErrLoggedOut)
	return nil
}

// WA — клиент whatsmeow, пока соединение есть (иначе ошибка).
func (c *Client) WA(ctx context.Context) (*whatsmeow.Client, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		c.mu.Lock()
		wa, ok := c.wa, c.connected
		c.mu.Unlock()
		if wa != nil && ok && wa.IsLoggedIn() {
			return wa, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("нет связи с WhatsApp (служба переподключается)")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Connected — есть ли сейчас связь.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wa != nil && c.connected
}

// Self — свой JID (номер телефона) без устройства.
func (c *Client) Self() types.JID {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wa == nil || c.wa.Store.ID == nil {
		return types.EmptyJID
	}
	return c.wa.Store.ID.ToNonAD()
}

// IsSelfChat — чат с самим собой (заметки себе).
func (c *Client) IsSelfChat(ch Chat) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wa == nil {
		return false
	}
	st := c.wa.Store
	return (st.ID != nil && st.ID.ToNonAD().String() == ch.JID) || (!st.LID.IsEmpty() && st.LID.ToNonAD().String() == ch.JID)
}

func (c *Client) handle(ctx context.Context, evt any) {
	defer func() {
		if r := recover(); r != nil {
			c.log("wa_handler_panic", "error", fmt.Sprint(r))
		}
	}()
	switch v := evt.(type) {
	case *events.Connected:
		c.mu.Lock()
		c.connected = true
		c.mu.Unlock()
		c.log("wa_connected")
	case *events.Disconnected:
		c.mu.Lock()
		c.connected = false
		c.mu.Unlock()
		c.log("wa_disconnected")
	case *events.PairSuccess:
		c.log("wa_paired")
		if c.Pair != nil && c.Pair.OnPaired != nil {
			c.Pair.OnPaired(v.ID.User)
		}
	case *events.LoggedOut:
		c.log("wa_logged_out", "reason", v.Reason.String())
		c.fail(ErrLoggedOut)
	case *events.StreamReplaced:
		c.fail(ErrReplaced)
	case *events.ClientOutdated:
		c.log("wa_client_outdated", "version", store.GetWAVersion().String())
		c.fail(ErrClientOutdated)
	case *events.TemporaryBan:
		c.log("wa_temporary_ban", "code", v.Code.String(), "expire", v.Expire.String())
	case *events.Message:
		c.ingest(ctx, v, true, false)
	case *events.HistorySync:
		c.history(ctx, v)
	case *events.AppStateSyncComplete:
		c.log("wa_appstate_synced", "name", string(v.Name), "version", v.Version)
		c.forgetNames() // контакты при полной синхронизации — одним событием в конце
	case *events.AppStateSyncError:
		c.log("wa_appstate_failed", "name", string(v.Name), "error", v.Error.Error())
	case *events.PushName, *events.Contact:
		c.forgetNames()
	case *events.GroupInfo:
		if v.Name != nil {
			if id, err := c.Store.ChatID(ctx, v.JID.String(), "group"); err == nil {
				c.Store.SetChatName(ctx, id, v.Name.Name)
			}
		}
	case *events.JoinedGroup:
		if id, err := c.Store.ChatID(ctx, v.JID.String(), "group"); err == nil {
			c.Store.SetChatName(ctx, id, v.Name)
		}
	case *events.Receipt:
		if v.IsFromMe && (v.Type == types.ReceiptTypeReadSelf || v.Type == types.ReceiptTypeRead) {
			c.markRead(ctx, v.Chat) // прочитал владелец (на телефоне)
		}
	case *events.MarkChatAsRead:
		if v.Action.GetRead() {
			c.markRead(ctx, v.JID)
		}
	}
}

func (c *Client) markRead(ctx context.Context, jid types.JID) {
	primary, alts := c.identities(ctx, jid)
	for _, j := range append([]string{primary}, alts...) {
		if ch, err := c.Store.ChatByJID(ctx, j); err == nil {
			c.Store.SetUnread(ctx, ch.ID, 0)
			return
		}
	}
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	lost := c.lost
	c.mu.Unlock()
	if lost != nil {
		select {
		case lost <- err:
		default:
		}
	}
}

// identities — JID чата одним видом (личка — по номеру телефона, если он
// известен) и другие его JID: WhatsApp шлёт одни и те же лички то по номеру,
// то по скрытому LID, а сопоставление узнаёт не сразу.
func (c *Client) identities(ctx context.Context, jid types.JID, alts ...types.JID) (string, []string) {
	jid = jid.ToNonAD()
	if jid.Server != types.HiddenUserServer && jid.Server != types.DefaultUserServer {
		return jid.String(), nil // группа, канал
	}
	var lid, pn types.JID
	for _, j := range append([]types.JID{jid}, alts...) {
		j = j.ToNonAD()
		switch j.Server {
		case types.HiddenUserServer:
			lid = j
		case types.DefaultUserServer:
			pn = j
		}
	}
	c.mu.Lock()
	wa := c.wa
	c.mu.Unlock()
	if wa != nil {
		if pn.IsEmpty() && !lid.IsEmpty() {
			if p, err := wa.Store.LIDs.GetPNForLID(ctx, lid); err == nil && !p.IsEmpty() {
				pn = p.ToNonAD()
			}
		}
		if lid.IsEmpty() && !pn.IsEmpty() {
			if l, err := wa.Store.LIDs.GetLIDForPN(ctx, pn); err == nil && !l.IsEmpty() {
				lid = l.ToNonAD()
			}
		}
	}
	primary := jid.String()
	if !pn.IsEmpty() {
		primary = pn.String()
	}
	var other []string
	for _, j := range []types.JID{pn, lid, jid} {
		if s := j.String(); !j.IsEmpty() && s != primary && !slices.Contains(other, s) {
			other = append(other, s)
		}
	}
	return primary, other
}

// KindOf — вид чата по JID.
func KindOf(jid string) string {
	switch {
	case strings.HasSuffix(jid, "@"+types.GroupServer):
		return "group"
	case strings.HasSuffix(jid, "@"+types.NewsletterServer):
		return "channel"
	case strings.HasSuffix(jid, "@"+types.BroadcastServer):
		return "broadcast"
	}
	return "user"
}

// chatOf — номер чата сообщения (с другими JID лички).
func (c *Client) chatOf(ctx context.Context, info *types.MessageInfo) (int64, error) {
	var alts []types.JID
	if !info.IsGroup {
		if info.IsFromMe {
			alts = append(alts, info.RecipientAlt)
		} else {
			alts = append(alts, info.SenderAlt, info.Sender)
		}
	}
	primary, other := c.identities(ctx, info.Chat, alts...)
	return c.Store.ChatID(ctx, primary, KindOf(primary), other...)
}

// ingest — сообщение в хранилище; live — пришло сейчас (будит подписки);
// edit — из истории пришла правка (ParseWebMessage уже подставил id
// исходного сообщения и новое содержимое).
func (c *Client) ingest(ctx context.Context, evt *events.Message, live, edit bool) {
	info := evt.Info
	if info.Chat == types.StatusBroadcastJID || info.Chat.Server == types.BroadcastServer {
		return // статусы и рассылки — не чаты
	}
	chatID, err := c.chatOf(ctx, &info)
	if err != nil {
		c.log("wa_store_failed", "error", err.Error())
		return
	}
	if !info.IsGroup && !info.IsFromMe && info.PushName != "" {
		c.Store.SetChatName(ctx, chatID, info.PushName) // запасное: имя из контактов главнее (Name)
	}
	msg := evt.Message
	sender, _ := c.identities(ctx, info.Sender, info.SenderAlt)
	emit := func(kind EventKind, ids ...int64) {
		if !live || c.OnEvent == nil || len(ids) == 0 || ids[0] == 0 {
			return
		}
		ch, err := c.Store.Chat(ctx, chatID)
		if err != nil {
			return
		}
		c.OnEvent(Event{Kind: kind, Chat: ch, IDs: ids, FromMe: info.IsFromMe})
	}
	if pm := msg.GetProtocolMessage(); pm != nil {
		switch pm.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			emit(EventDelete, c.Store.Delete(ctx, chatID, pm.GetKey().GetID(), info.Timestamp))
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			edited := pm.GetEditedMessage()
			emit(EventEdit, c.Store.Edit(ctx, chatID, pm.GetKey().GetID(), Parse(edited).Text, editRaw(edited), info.Timestamp))
		}
		return
	}
	if r := msg.GetReactionMessage(); r != nil {
		at := info.Timestamp // с точностью до секунды — у истории миллисекунды
		if ms := r.GetSenderTimestampMS(); ms > 0 {
			at = time.UnixMilli(ms)
		}
		c.Store.React(ctx, chatID, r.GetKey().GetID(), sender, r.GetText(), at)
		return
	}
	content := Parse(msg)
	if content.Empty() {
		return
	}
	if edit {
		c.Store.Edit(ctx, chatID, info.ID, content.Text, editRaw(msg), info.Timestamp)
		return
	}
	raw, _ := proto.Marshal(msg)
	m := &Message{Chat: chatID, WAID: info.ID, Sender: sender, FromMe: info.IsFromMe, Time: info.Timestamp,
		Text: content.Text, Kind: content.Kind, Raw: raw, PushName: info.PushName, Live: live}
	if ci := content.Context; ci != nil {
		m.ReplyTo, m.Forward = ci.GetStanzaID(), ci.GetIsForwarded()
	}
	id, fresh, err := c.Store.Put(ctx, m)
	if err != nil {
		c.log("wa_store_failed", "error", err.Error())
		return
	}
	if web := evt.SourceWebMsg; web != nil { // реакции из истории
		var peer string // собеседник лички — автор чужих реакций в ней
		if !info.IsGroup {
			peer, _ = c.identities(ctx, info.Chat)
		}
		for _, r := range web.GetReactions() {
			// автор реакции — не автор сообщения: на своё сообщение реагирует собеседник
			who := ""
			switch p := r.GetKey().GetParticipant(); {
			case r.GetKey().GetFromMe():
				who = c.Self().String()
			case p != "":
				if j, err := types.ParseJID(p); err == nil {
					who, _ = c.identities(ctx, j)
				}
			case peer != "":
				who = peer
			}
			if who == "" {
				continue // автора не узнать — лучше без реакции, чем чужая
			}
			c.Store.React(ctx, chatID, info.ID, who, r.GetText(), time.UnixMilli(r.GetSenderTimestampMS()))
		}
	}
	if fresh {
		emit(EventNew, id)
	}
}

// editRaw — содержимое правки для хранилища: у вложения правится только
// подпись, а ключи файла — в прежнем raw (nil — не трогать).
func editRaw(m *waE2E.Message) []byte {
	if Parse(m).Kind == "" {
		return nil
	}
	raw, _ := proto.Marshal(m)
	return raw
}

// history — история при входе (и по запросу): в хранилище, подписки не будит.
func (c *Client) history(ctx context.Context, v *events.HistorySync) {
	c.mu.Lock()
	wa := c.wa
	c.mu.Unlock()
	if wa == nil || v.Data == nil {
		return
	}
	n := 0
	for _, conv := range v.Data.GetConversations() {
		jid, err := types.ParseJID(conv.GetID())
		if err != nil || jid.Server == types.BroadcastServer {
			continue
		}
		var alts []types.JID
		for _, s := range []string{conv.GetPnJID(), conv.GetLidJID()} {
			if j, err := types.ParseJID(s); err == nil && s != "" {
				alts = append(alts, j)
			}
		}
		primary, other := c.identities(ctx, jid, alts...)
		chatID, err := c.Store.ChatID(ctx, primary, KindOf(primary), other...)
		if err != nil {
			continue
		}
		c.Store.SetChatName(ctx, chatID, conv.GetName())
		for _, hm := range conv.GetMessages() {
			web := hm.GetMessage()
			evt, err := wa.ParseWebMessage(jid, web)
			if err != nil {
				continue
			}
			c.ingest(ctx, evt, false, evt.Info.ID != web.GetKey().GetID())
			n++
		}
		if conv.UnreadCount != nil {
			c.Store.SetUnread(ctx, chatID, int(conv.GetUnreadCount()))
		}
	}
	c.log("wa_history_sync", "type", v.Data.GetSyncType().String(), "chats", len(v.Data.GetConversations()), "messages", n)
}

func (c *Client) forgetNames() {
	c.mu.Lock()
	c.names = nil
	c.mu.Unlock()
}

// Name — имя человека: из контактов телефона, иначе как он сам назвался,
// иначе номер.
func (c *Client) Name(ctx context.Context, jidStr, pushName string) string {
	c.mu.Lock()
	if n, ok := c.names[jidStr]; ok {
		c.mu.Unlock()
		return n
	}
	wa := c.wa
	c.mu.Unlock()
	jid, err := types.ParseJID(jidStr)
	if err != nil {
		return jidStr
	}
	if jid.Server == types.HiddenUserServer {
		if p, _ := c.identities(ctx, jid); p != jidStr {
			if pj, err := types.ParseJID(p); err == nil {
				jid = pj
			}
		}
	}
	name := ""
	if wa != nil {
		if ct, err := wa.Store.Contacts.GetContact(ctx, jid); err == nil && ct.Found {
			for _, n := range []string{ct.FullName, ct.FirstName, ct.BusinessName, ct.PushName} {
				if strings.TrimSpace(n) != "" {
					name = n
					break
				}
			}
		}
	}
	if name == "" && pushName != "" {
		return pushName // не запоминаем: из контактов может появиться настоящее
	}
	if name == "" {
		// номер не запоминаем: контакты телефона догружаются после входа
		if jid.Server == types.DefaultUserServer {
			return "+" + jid.User
		}
		return jid.User
	}
	c.mu.Lock()
	if c.names == nil {
		c.names = map[string]string{}
	}
	c.names[jidStr] = name
	c.mu.Unlock()
	return name
}

// ChatTitle — название чата: у группы — её, у лички — имя собеседника.
func (c *Client) ChatTitle(ctx context.Context, ch Chat) string {
	if ch.Kind == "user" {
		if c.IsSelfChat(ch) {
			return "Я (заметки себе)"
		}
		return c.Name(ctx, ch.JID, ch.Name)
	}
	if ch.Name != "" {
		return ch.Name
	}
	if ch.Kind == "group" {
		if wa, err := c.WA(ctx); err == nil {
			jid, _ := types.ParseJID(ch.JID)
			if gi, err := wa.GetGroupInfo(ctx, jid); err == nil && gi.Name != "" {
				c.Store.SetChatName(ctx, ch.ID, gi.Name)
				return gi.Name
			}
		}
	}
	return ch.JID
}

// SyncAppState — догрузить состояние приложения (контакты телефона, их
// имена), если оно ещё не загружено: whatsmeow берёт его только когда
// приходят ключи, и сбой той загрузки ничем не повторяется. Уже загруженное
// не перекачивается.
func (c *Client) SyncAppState(ctx context.Context) error {
	wa, err := c.WA(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range appstate.AllPatchNames {
		if err := wa.FetchAppState(ctx, name, false, true); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	c.forgetNames()
	return errors.Join(errs...)
}

// SyncGroups — названия всех групп аккаунта (после входа: в истории их нет).
func (c *Client) SyncGroups(ctx context.Context) (int, error) {
	wa, err := c.WA(ctx)
	if err != nil {
		return 0, err
	}
	groups, err := wa.GetJoinedGroups(ctx)
	if err != nil {
		return 0, err
	}
	for _, g := range groups {
		if id, err := c.Store.ChatID(ctx, g.JID.String(), "group"); err == nil {
			c.Store.SetChatName(ctx, id, g.Name)
		}
	}
	return len(groups), nil
}

// Version — версия веб-клиента WhatsApp, которой представляется шлюз.
func Version() string { return store.GetWAVersion().String() }
