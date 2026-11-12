package wac

// Сообщения WhatsApp на диске шлюза. Истории чатов на сервере WhatsApp
// нет: устройство получает её один раз при входе (history sync) и дальше —
// только новые сообщения. Поэтому всё, что пришло, служба складывает сюда, а
// чтение агентом идёт отсюда. Агенту id сообщения и чата — номера строк
// (маленькие целые, как в Telegram), id WhatsApp (строки) — только внутри.
//
// Два порядка. Показ и листание назад (before_id, последние) — по времени
// сообщения. «Что нового после id» (after_id, ленты и подписки) — по номеру
// строки, то есть по приходу, и только живые сообщения (live): история при
// входе приходит позже живых, у неё большие номера и старое время — подписки
// она не будит и в «новое» не попадает.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // драйвер "sqlite" без cgo
)

// Chat — чат аккаунта, какой его видел шлюз.
type Chat struct {
	ID     int64  `json:"id"`
	JID    string `json:"jid"`
	Name   string `json:"name"`
	Kind   string `json:"kind"` // user, group, channel, broadcast
	LastTS int64  `json:"last_ts"`
	Unread int    `json:"unread"`
}

// Message — сообщение в хранилище.
type Message struct {
	ID       int64
	Chat     int64
	ChatJID  string
	WAID     string
	Sender   string // JID автора без устройства
	FromMe   bool
	Time     time.Time
	Text     string
	Kind     string // вид вложения словами Telegram: photo, voice, …; пусто — текст
	Raw      []byte // waE2E.Message (protobuf): скачать вложение, процитировать
	ReplyTo  string // WAID сообщения, на которое ответ
	Edited   time.Time
	Deleted  bool
	PushName string
	Forward  bool
	Live     bool // пришло событием, а не с историей
	// Reactions — реакции: автор → эмодзи (заполняет Get/List)
	Reactions map[string]string
}

// Store — база сообщений (data/wa/messages.db). mu упорядочивает запись:
// живые события и история идут из разных горутин, а «нет сообщения —
// отложить изменение» и «положить сообщение — применить отложенное» должны
// не разминуться.
type Store struct {
	db *sql.DB
	mu sync.Mutex
	// owed — отложенное, которое уже застало своё сообщение, применить не
	// вышло (ошибка базы): следующая запись попробует снова (settle)
	owed bool
	// unsaved — изменения, которые не удалось даже положить в pending:
	// держатся в памяти, пока settle их не запишет
	unsaved []pendingChange
}

type pendingChange struct {
	chat                      int64
	waID, kind, sender, value string
	raw                       []byte
	at                        time.Time
}

const schema = `
-- AUTOINCREMENT: номер удалённой (слитой) строки не достаётся новой — на
-- него ведут белый список, ленты и merged
CREATE TABLE IF NOT EXISTS chats(
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	jid TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL DEFAULT '',
	last_ts INTEGER NOT NULL DEFAULT 0,
	unread INTEGER NOT NULL DEFAULT 0
);
-- другие JID того же чата: личку WhatsApp шлёт то по номеру, то по LID
CREATE TABLE IF NOT EXISTS aliases(
	jid TEXT PRIMARY KEY,
	chat INTEGER NOT NULL
);
-- чат, слитый с другим (одна личка успела завестись под номером и под LID):
-- его номер остаётся рабочим — им мог быть открыт доступ
CREATE TABLE IF NOT EXISTS merged(
	old INTEGER PRIMARY KEY,
	into_id INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS messages(
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	chat INTEGER NOT NULL,
	wa_id TEXT NOT NULL,
	sender TEXT NOT NULL DEFAULT '',
	from_me INTEGER NOT NULL DEFAULT 0,
	ts INTEGER NOT NULL,
	text TEXT NOT NULL DEFAULT '',
	text_lc TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL DEFAULT '',
	raw BLOB,
	reply_to TEXT NOT NULL DEFAULT '',
	edited INTEGER NOT NULL DEFAULT 0,
	deleted INTEGER NOT NULL DEFAULT 0,
	push_name TEXT NOT NULL DEFAULT '',
	forward INTEGER NOT NULL DEFAULT 0,
	live INTEGER NOT NULL DEFAULT 0,
	UNIQUE(chat, wa_id)
);
CREATE INDEX IF NOT EXISTS messages_chat_ts ON messages(chat, ts, id);
CREATE TABLE IF NOT EXISTS reactions(
	msg INTEGER NOT NULL,
	sender TEXT NOT NULL,
	emoji TEXT NOT NULL,
	ts INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(msg, sender)
);
-- правки, удаления и реакции, пришедшие раньше самого сообщения (история
-- догружается позже живых): применяются, когда сообщение появится
CREATE TABLE IF NOT EXISTS pending(
	chat INTEGER NOT NULL,
	wa_id TEXT NOT NULL,
	kind TEXT NOT NULL,
	sender TEXT NOT NULL DEFAULT '',
	value TEXT NOT NULL DEFAULT '',
	raw BLOB,
	ts INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS pending_msg ON pending(chat, wa_id);`

// OpenStore — открыть (или завести) базу сообщений.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("wac: база сообщений %s: %w", path, err)
	}
	st := &Store{db: db}
	st.settleAll(context.Background()) // прошлый процесс мог не успеть
	return st, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ChatID — номер чата по JID и его другим JID (номер ↔ LID): чат, известный
// под любым из них, остаётся тем же номером. Не было ни под одним —
// заводится под jid. Оказалось, что под разными JID завелись разные чаты
// (сопоставление стало известно позже), — они сливаются в первый, а номер
// второго продолжает вести на него (Resolve).
func (s *Store) ChatID(ctx context.Context, jid, kind string, alts ...string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := append([]string{jid}, alts...)
	var found []int64
	for _, j := range ids {
		if j == "" {
			continue
		}
		rows, err := s.db.QueryContext(ctx, `SELECT id FROM chats WHERE jid = ? UNION SELECT chat FROM aliases WHERE jid = ?`, j, j)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil && !slices.Contains(found, id) {
				found = append(found, id)
			}
		}
		rows.Close()
	}
	var id int64
	if len(found) == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO chats(jid, kind) VALUES(?, ?)`, jid, kind)
		if err != nil {
			return 0, err
		}
		if id, err = res.LastInsertId(); err != nil {
			return 0, err
		}
	} else {
		slices.Sort(found)
		id = found[0]
		for _, other := range found[1:] {
			if err := s.merge(ctx, other, id); err != nil {
				return 0, err
			}
		}
	}
	for _, j := range ids {
		if j != "" {
			_, _ = s.db.ExecContext(ctx, `INSERT INTO aliases(jid, chat) SELECT ?, ? WHERE NOT EXISTS (SELECT 1 FROM chats WHERE jid = ?)
				ON CONFLICT(jid) DO NOTHING`, j, id, j)
		}
	}
	return id, nil
}

// merge — чат other в чат into: сообщения, отложенное, псевдонимы; номер
// other ведёт на into.
func (s *Store) merge(ctx context.Context, other, into int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var otherJID string
	if err := tx.QueryRowContext(ctx, `SELECT jid FROM chats WHERE id = ?`, other).Scan(&otherJID); err != nil {
		return err
	}
	// одно и то же сообщение в обоих чатах: остаётся строка into, но с самым
	// свежим состоянием (удалено — значит удалено, правка — новейшая), и
	// реакции переезжают на неё (по автору — новейшая, снятые тоже)
	type dup struct{ keep, drop int64 }
	var dups []dup
	rows, err := tx.QueryContext(ctx, `SELECT k.id, o.id FROM messages o JOIN messages k ON k.chat = ? AND k.wa_id = o.wa_id WHERE o.chat = ?`, into, other)
	if err != nil {
		return err
	}
	for rows.Next() {
		var d dup
		if rows.Scan(&d.keep, &d.drop) == nil {
			dups = append(dups, d)
		}
	}
	rows.Close()
	for _, d := range dups {
		for _, st := range []struct {
			q    string
			args []any
		}{
			// удалено в любой из строк — удалено (и правка его не вернёт)
			{`UPDATE messages SET deleted = 1, text = '', text_lc = '', raw = NULL
				WHERE id = ? AND (deleted = 1 OR (SELECT deleted FROM messages WHERE id = ?) = 1)`,
				[]any{d.keep, d.drop}},
			// правка новее — с той строки, если сообщение не удалено
			{`UPDATE messages SET text = o.text, text_lc = o.text_lc, raw = o.raw, edited = o.edited
				FROM (SELECT text, text_lc, raw, edited FROM messages WHERE id = ?) o
				WHERE messages.id = ? AND messages.deleted = 0 AND o.edited > messages.edited`,
				[]any{d.drop, d.keep}},
			// реакции — на остающуюся, по автору новейшая (снятые тоже)
			{`INSERT INTO reactions(msg, sender, emoji, ts) SELECT ?, sender, emoji, ts FROM reactions WHERE msg = ? AND true
				ON CONFLICT(msg, sender) DO UPDATE SET emoji = excluded.emoji, ts = excluded.ts WHERE excluded.ts >= reactions.ts`,
				[]any{d.keep, d.drop}},
			{`DELETE FROM reactions WHERE msg = ?`, []any{d.drop}},
		} {
			if _, err := tx.ExecContext(ctx, st.q, st.args...); err != nil {
				return fmt.Errorf("wac: слияние сообщений %d → %d: %w", d.drop, d.keep, err)
			}
		}
	}
	stmts := []struct {
		q    string
		args []any
	}{
		{`UPDATE OR IGNORE messages SET chat = ? WHERE chat = ?`, []any{into, other}},
		{`DELETE FROM messages WHERE chat = ?`, []any{other}},
		{`UPDATE pending SET chat = ? WHERE chat = ?`, []any{into, other}},
		{`UPDATE aliases SET chat = ? WHERE chat = ?`, []any{into, other}},
		{`INSERT INTO aliases(jid, chat) VALUES(?, ?) ON CONFLICT(jid) DO UPDATE SET chat = excluded.chat`, []any{otherJID, into}},
		{`UPDATE chats SET last_ts = max(last_ts, (SELECT last_ts FROM chats WHERE id = ?)),
			unread = unread + (SELECT unread FROM chats WHERE id = ?),
			name = CASE WHEN name = '' THEN (SELECT name FROM chats WHERE id = ?) ELSE name END WHERE id = ?`, []any{other, other, other, into}},
		{`DELETE FROM chats WHERE id = ?`, []any{other}},
		{`UPDATE merged SET into_id = ? WHERE into_id = ?`, []any{into, other}},
		{`INSERT INTO merged(old, into_id) VALUES(?, ?) ON CONFLICT(old) DO UPDATE SET into_id = excluded.into_id`, []any{other, into}},
	}
	for _, st := range stmts {
		if _, err := tx.ExecContext(ctx, st.q, st.args...); err != nil {
			return fmt.Errorf("wac: слияние чатов %d → %d: %w", other, into, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// отложенное, чьё сообщение было во втором чате, теперь застаёт его;
	// не вышло — повторит следующая запись (owed), прервали — открытие базы
	s.settleAll(ctx)
	return nil
}

// settleAll — применить всё готовое отложенное; не вышло — запомнить долг.
func (s *Store) settleAll(ctx context.Context) {
	// в pending — только начало очереди до первой неудачи: иначе более
	// позднее изменение применилось бы раньше застрявшего
	n := 0
	for _, c := range s.unsaved {
		chat, err := s.resolve(ctx, c.chat) // чат мог слиться, пока изменение ждало
		if err == nil {
			c.chat = chat
			err = s.insertPending(ctx, c)
		}
		if err != nil {
			break
		}
		n++
	}
	s.unsaved = s.unsaved[n:]
	if len(s.unsaved) == 0 {
		s.unsaved = nil
	}
	s.owed = s.drainReady(ctx) != nil || len(s.unsaved) > 0
}

// settle — вернуть долг прошлой неудачной попытки, если он есть.
func (s *Store) settle(ctx context.Context) {
	if s.owed {
		s.settleAll(ctx)
	}
}

// drainReady — применить всё отложенное, чьё сообщение уже есть.
func (s *Store) drainReady(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT p.chat, p.wa_id FROM pending p JOIN messages m ON m.chat = p.chat AND m.wa_id = p.wa_id`)
	if err != nil {
		return err
	}
	type key struct {
		chat int64
		wa   string
	}
	var ready []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.chat, &k.wa); err != nil {
			rows.Close()
			return err
		}
		ready = append(ready, k)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err // обход оборвался — долг не снимать
	}
	for _, k := range ready {
		if err := s.applyPending(ctx, k.chat, k.wa); err != nil {
			return err
		}
	}
	return nil
}

// resolve — как Resolve, но ошибка базы — ошибка, а не «не слит».
func (s *Store) resolve(ctx context.Context, id int64) (int64, error) {
	var into int64
	switch err := s.db.QueryRowContext(ctx, `SELECT into_id FROM merged WHERE old = ?`, id).Scan(&into); {
	case err == nil:
		return into, nil
	case errors.Is(err, sql.ErrNoRows):
		return id, nil
	default:
		return 0, err
	}
}

// Resolve — рабочий номер чата: слитый ведёт на тот, с которым слит.
func (s *Store) Resolve(ctx context.Context, id int64) int64 {
	var into int64
	if s.db.QueryRowContext(ctx, `SELECT into_id FROM merged WHERE old = ?`, id).Scan(&into) == nil {
		return into
	}
	return id
}

// SetChatName — название чата (пустое не затирает известное).
func (s *Store) SetChatName(ctx context.Context, chat int64, name string) {
	if strings.TrimSpace(name) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.ExecContext(ctx, `UPDATE chats SET name = ? WHERE id = ?`, name, s.Resolve(ctx, chat))
}

// SetUnread — непрочитанные чата (из истории при входе, 0 — прочитал владелец).
func (s *Store) SetUnread(ctx context.Context, chat int64, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.db.ExecContext(ctx, `UPDATE chats SET unread = ? WHERE id = ?`, n, s.Resolve(ctx, chat))
}

const chatCols = `id, jid, name, kind, last_ts, unread`

func scanChat(row interface{ Scan(...any) error }) (Chat, error) {
	var c Chat
	err := row.Scan(&c.ID, &c.JID, &c.Name, &c.Kind, &c.LastTS, &c.Unread)
	return c, err
}

// Chat — чат по номеру (и по номеру слитого с ним).
func (s *Store) Chat(ctx context.Context, id int64) (Chat, error) {
	c, err := scanChat(s.db.QueryRowContext(ctx, `SELECT `+chatCols+` FROM chats WHERE id = ?`, s.Resolve(ctx, id)))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNoChat
	}
	return c, err
}

// ChatByJID — чат по JID (или его другому JID).
func (s *Store) ChatByJID(ctx context.Context, jid string) (Chat, error) {
	c, err := scanChat(s.db.QueryRowContext(ctx, `SELECT `+chatCols+` FROM chats WHERE jid = ? OR id = (SELECT chat FROM aliases WHERE jid = ?)`, jid, jid))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNoChat
	}
	return c, err
}

// ErrNoChat — такого чата шлюз не видел.
var ErrNoChat = errors.New("wac: чата нет")

// Chats — все чаты, свежие сверху.
func (s *Store) Chats(ctx context.Context) ([]Chat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+chatCols+` FROM chats ORDER BY last_ts DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chat
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Put — положить сообщение; уже было (тот же WAID в чате) — его номер и
// false. m.Live — пришло сейчас, а не из истории: считается непрочитанным.
func (s *Store) Put(ctx context.Context, m *Message) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settle(ctx)
	m.Chat = s.Resolve(ctx, m.Chat) // номер мог слиться, пока его несли сюда
	res, err := s.db.ExecContext(ctx, `INSERT INTO messages(chat, wa_id, sender, from_me, ts, text, text_lc, kind, raw, reply_to, push_name, forward, live)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(chat, wa_id) DO NOTHING`,
		m.Chat, m.WAID, m.Sender, m.FromMe, m.Time.UnixMilli(), m.Text, strings.ToLower(m.Text), m.Kind, m.Raw, m.ReplyTo,
		m.PushName, m.Forward, m.Live)
	if err != nil {
		return 0, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var id int64
		err := s.db.QueryRowContext(ctx, `SELECT id FROM messages WHERE chat = ? AND wa_id = ?`, m.Chat, m.WAID).Scan(&id)
		m.ID = id
		if s.applyPending(ctx, m.Chat, m.WAID) != nil {
			s.owed = true
		}
		return id, false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, err
	}
	m.ID = id
	switch {
	case m.FromMe:
		// владелец написал в чат — значит, прочитал его
		_, err = s.db.ExecContext(ctx, `UPDATE chats SET last_ts = max(last_ts, ?), unread = 0 WHERE id = ?`, m.Time.UnixMilli(), m.Chat)
	case m.Live:
		_, err = s.db.ExecContext(ctx, `UPDATE chats SET last_ts = max(last_ts, ?), unread = unread + 1 WHERE id = ?`, m.Time.UnixMilli(), m.Chat)
	default:
		_, err = s.db.ExecContext(ctx, `UPDATE chats SET last_ts = max(last_ts, ?) WHERE id = ?`, m.Time.UnixMilli(), m.Chat)
	}
	if s.applyPending(ctx, m.Chat, m.WAID) != nil {
		s.owed = true
	}
	return id, true, err
}

// msgID — номер сообщения; 0 и nil — его ещё нет.
func (s *Store) msgID(ctx context.Context, chat int64, waID string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM messages WHERE chat = ? AND wa_id = ?`, chat, waID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// defer_ — положить изменение в pending; не вышло — в память до settle.
func (s *Store) defer_(ctx context.Context, c pendingChange) {
	if s.insertPending(ctx, c) != nil {
		s.unsaved = append(s.unsaved, c)
		s.owed = true
	}
}

func (s *Store) insertPending(ctx context.Context, c pendingChange) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pending(chat, wa_id, kind, sender, value, raw, ts) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		c.chat, c.waID, c.kind, c.sender, c.value, c.raw, c.at.UnixMilli())
	return err
}

// applyPending — отложенные изменения сообщения, которое только что
// появилось. Строка отложенного удаляется только после того, как изменение
// записано: прервали посередине — остаток применит следующий раз.
func (s *Store) applyPending(ctx context.Context, chat int64, waID string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT rowid, kind, sender, value, raw, ts FROM pending WHERE chat = ? AND wa_id = ? ORDER BY ts, rowid`, chat, waID)
	if err != nil {
		return err
	}
	type change struct {
		rowid               int64
		kind, sender, value string
		raw                 []byte
		ts                  int64
	}
	var list []change
	for rows.Next() {
		var c change
		if err := rows.Scan(&c.rowid, &c.kind, &c.sender, &c.value, &c.raw, &c.ts); err != nil {
			rows.Close()
			return err
		}
		list = append(list, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(list) == 0 {
		return err
	}
	var id int64
	switch err := s.db.QueryRowContext(ctx, `SELECT id FROM messages WHERE chat = ? AND wa_id = ?`, chat, waID).Scan(&id); {
	case errors.Is(err, sql.ErrNoRows):
		return nil // сообщения нет — ждать его
	case err != nil:
		return err
	}
	for _, c := range list {
		// изменение и снятие его из pending — одной транзакцией: иначе
		// повтор применил бы его снова поверх более позднего
		err := func() error {
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			pc := pendingChange{kind: c.kind, sender: c.sender, value: c.value, raw: c.raw, at: time.UnixMilli(c.ts)}
			if err := applyChange(ctx, tx, id, pc); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM pending WHERE rowid = ?`, c.rowid); err != nil {
				return err
			}
			return tx.Commit()
		}()
		if err != nil {
			return fmt.Errorf("wac: отложенное изменение %s: %w", c.kind, err)
		}
	}
	return nil
}

func applyChange(ctx context.Context, db execer, id int64, c pendingChange) error {
	switch c.kind {
	case "edit":
		return applyEdit(ctx, db, id, c.value, c.raw, c.at.UnixMilli())
	case "delete":
		return applyDelete(ctx, db, id)
	case "react":
		return applyReact(ctx, db, id, c.sender, c.value, c.at.UnixMilli())
	}
	return fmt.Errorf("wac: неизвестное изменение %q", c.kind)
}

// execer — база или транзакция.
type execer interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

func applyEdit(ctx context.Context, db execer, id int64, text string, raw []byte, ts int64) error {
	var err error
	if raw != nil {
		_, err = db.ExecContext(ctx, `UPDATE messages SET text = ?, text_lc = ?, raw = ?, edited = ? WHERE id = ? AND edited <= ? AND deleted = 0`,
			text, strings.ToLower(text), raw, ts, id, ts)
	} else {
		_, err = db.ExecContext(ctx, `UPDATE messages SET text = ?, text_lc = ?, edited = ? WHERE id = ? AND edited <= ? AND deleted = 0`,
			text, strings.ToLower(text), ts, id, ts)
	}
	return err
}

func applyDelete(ctx context.Context, db execer, id int64) error {
	_, err := db.ExecContext(ctx, `UPDATE messages SET deleted = 1, text = '', text_lc = '', raw = NULL WHERE id = ?`, id)
	return err
}

func applyReact(ctx context.Context, db execer, id int64, sender, emoji string, ts int64) error {
	_, err := db.ExecContext(ctx, `INSERT INTO reactions(msg, sender, emoji, ts) VALUES(?, ?, ?, ?)
		ON CONFLICT(msg, sender) DO UPDATE SET emoji = excluded.emoji, ts = excluded.ts WHERE excluded.ts >= reactions.ts`,
		id, sender, emoji, ts)
	return err
}

// Edit — новый текст сообщения; номер сообщения (0 — его ещё нет: правка
// отложена до него). Правка старше уже применённой не применяется.
func (s *Store) Edit(ctx context.Context, chat int64, waID, text string, raw []byte, at time.Time) int64 {
	return s.change(ctx, pendingChange{chat: chat, waID: waID, kind: "edit", value: text, raw: raw, at: at})
}

// Delete — сообщение удалено у всех; номер (0 — его ещё нет: удаление
// отложено, и пришедшее позже сообщение сразу будет удалённым).
func (s *Store) Delete(ctx context.Context, chat int64, waID string, at time.Time) int64 {
	return s.change(ctx, pendingChange{chat: chat, waID: waID, kind: "delete", at: at})
}

// React — реакция автора на сообщение (пустая — снял); номер сообщения
// (0 — его ещё нет: отложено). Реакция старше известной не применяется;
// снятая хранится пустой со временем — иначе старая история вернула бы её.
func (s *Store) React(ctx context.Context, chat int64, waID, sender, emoji string, at time.Time) int64 {
	return s.change(ctx, pendingChange{chat: chat, waID: waID, kind: "react", sender: sender, value: emoji, at: at})
}

// change — применить изменение сообщения. Нет сообщения — в pending до
// него. Ошибка базы — тоже в pending (или в память) с долгом: повторит
// следующая запись. Есть невозвращённый долг — изменение идёт через
// pending, чтобы порядок с застрявшими (по времени, потом по приходу) не
// нарушился. Контекст без отмены: истёкший контекст вызова не должен
// потерять изменение.
func (s *Store) change(ctx context.Context, c pendingChange) int64 {
	ctx = context.WithoutCancel(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settle(ctx) // застрявшие — раньше этого
	if len(s.unsaved) > 0 {
		s.unsaved = append(s.unsaved, c) // в базу всё ещё не пишется — в очередь за ними
		return 0
	}
	chat, err := s.resolve(ctx, c.chat)
	if err != nil {
		s.unsaved = append(s.unsaved, c) // settle разрешит чат снова
		s.owed = true
		return 0
	}
	c.chat = chat
	id, err := s.msgID(ctx, chat, c.waID)
	switch {
	case err != nil:
		s.defer_(ctx, c)
		s.owed = true
		return 0
	case id == 0:
		s.defer_(ctx, c)
		return 0
	case s.owed:
		s.defer_(ctx, c)
		if s.applyPending(ctx, chat, c.waID) != nil {
			s.owed = true
		}
		return id
	}
	if applyChange(ctx, s.db, id, c) != nil {
		s.defer_(ctx, c)
		s.owed = true
	}
	return id
}

const msgCols = `m.id, m.chat, c.jid, m.wa_id, m.sender, m.from_me, m.ts, m.text, m.kind, m.raw, m.reply_to, m.edited, m.deleted, m.push_name, m.forward, m.live`

func scanMsg(row interface{ Scan(...any) error }) (Message, error) {
	var (
		m          Message
		ts, edited int64
	)
	err := row.Scan(&m.ID, &m.Chat, &m.ChatJID, &m.WAID, &m.Sender, &m.FromMe, &ts, &m.Text, &m.Kind, &m.Raw,
		&m.ReplyTo, &edited, &m.Deleted, &m.PushName, &m.Forward, &m.Live)
	m.Time = time.UnixMilli(ts)
	if edited > 0 {
		m.Edited = time.UnixMilli(edited)
	}
	return m, err
}

func (s *Store) list(ctx context.Context, where string, args ...any) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+msgCols+` FROM messages m JOIN chats c ON c.id = m.chat WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.reactions(ctx, out)
}

func (s *Store) reactions(ctx context.Context, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	idx := map[int64]int{}
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		idx[m.ID] = i
		ids[i] = fmt.Sprint(m.ID)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT msg, sender, emoji FROM reactions WHERE emoji != '' AND msg IN (`+strings.Join(ids, ",")+`)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id            int64
			sender, emoji string
		)
		if err := rows.Scan(&id, &sender, &emoji); err != nil {
			return err
		}
		m := &msgs[idx[id]]
		if m.Reactions == nil {
			m.Reactions = map[string]string{}
		}
		m.Reactions[sender] = emoji
	}
	return rows.Err()
}

// Get — сообщение по номеру.
func (s *Store) Get(ctx context.Context, id int64) (*Message, error) {
	list, err := s.list(ctx, `m.id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNoMessage
	}
	return &list[0], nil
}

// ByWAID — сообщение чата по id WhatsApp.
func (s *Store) ByWAID(ctx context.Context, chat int64, waID string) (*Message, error) {
	list, err := s.list(ctx, `m.chat = ? AND m.wa_id = ?`, chat, waID)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNoMessage
	}
	return &list[0], nil
}

// ErrNoMessage — такого сообщения в хранилище нет.
var ErrNoMessage = errors.New("wac: сообщения нет")

// Query — что выбрать из чата.
type Query struct {
	Before int64 // по времени: раньше этого сообщения
	After  int64 // по приходу: живые, пришедшие после этого номера
	Live   bool  // только живые (без истории при входе)
	Since  time.Time
	Search string
	Limit  int
}

// History — сообщения чата (удалённые не показываются). С After — по
// возрастанию номера (порядок прихода, как у лент), иначе — последние по
// времени, по возрастанию времени.
func (s *Store) History(ctx context.Context, chat int64, q Query) ([]Message, error) {
	chat = s.Resolve(ctx, chat)
	where := []string{`m.chat = ?`, `m.deleted = 0`}
	args := []any{chat}
	if q.Before > 0 {
		var ts int64
		if err := s.db.QueryRowContext(ctx, `SELECT ts FROM messages WHERE id = ?`, q.Before).Scan(&ts); err != nil {
			return nil, fmt.Errorf("сообщения %d нет", q.Before)
		}
		where = append(where, `(m.ts < ? OR (m.ts = ? AND m.id < ?))`)
		args = append(args, ts, ts, q.Before)
	}
	if q.After > 0 {
		where = append(where, `m.id > ?`, `m.live = 1`)
		args = append(args, q.After)
	}
	if q.Live {
		where = append(where, `m.live = 1`)
	}
	if !q.Since.IsZero() {
		where = append(where, `m.ts >= ?`)
		args = append(args, q.Since.UnixMilli())
	}
	if q.Search != "" {
		// strings.ToLower: LIKE в SQLite не знает регистра кириллицы
		where = append(where, `m.text_lc LIKE ? ESCAPE '\'`)
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(q.Search))
		args = append(args, "%"+esc+"%")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	if q.After > 0 {
		return s.list(ctx, strings.Join(where, ` AND `)+` ORDER BY m.id ASC LIMIT ?`, append(args, limit)...)
	}
	list, err := s.list(ctx, strings.Join(where, ` AND `)+` ORDER BY m.ts DESC, m.id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
	return list, nil
}
