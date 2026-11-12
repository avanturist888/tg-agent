package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"tgagent/internal/audit"
	"tgagent/internal/lock"
	"tgagent/internal/store"
)

// Outbox — очередь черновиков профиля: таблица drafts в state.db. Файл
// общий для всех процессов профиля; каждое изменение — одна транзакция
// записи (store.Write), так что «прочитать → проверить статус → изменить»
// атомарно и между процессами.
type Outbox struct{ Dir string }

// New — очередь в data/ профиля (dir).
func New(dir string) *Outbox { return &Outbox{Dir: dir} }

// keep — сколько решённых черновиков хранить (нерешённые не выбрасываются:
// они ждут решения владельца, а протухшие сами становятся решёнными).
const keep = 200

func (o *Outbox) db() (*store.DB, error) { return store.Open(o.Dir) }

func decode(body string) (*Draft, error) {
	d := &Draft{}
	if err := json.Unmarshal([]byte(body), d); err != nil {
		return nil, err
	}
	return d, nil
}

// touch — статус с учётом срока: протухший нерешённый — expired. В базу
// это попадает с ближайшей записью (expire), чтения ничего не пишут.
func touch(d *Draft, now time.Time) *Draft {
	if d.IsExpired(now) {
		d.Status = Expired
	}
	return d
}

func unixPtr(s *string) any {
	if s == nil {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return nil
	}
	return t.Unix()
}

func unixOf(s string) int64 {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// save — записать черновик (столбцы для выборок — из него же).
func save(ctx context.Context, tx *sql.Tx, d *Draft) error {
	body, err := json.Marshal(d)
	if err != nil {
		return err
	}
	var sendAt any
	if d.Status == Scheduled {
		sendAt = unixPtr(d.SendAt)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO drafts(id, status, expires, send_at, body) VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET status = excluded.status, expires = excluded.expires,
			send_at = excluded.send_at, body = excluded.body`,
		d.ID, d.Status, unixOf(d.ExpiresAt), sendAt, string(body))
	return err
}

// expire — записать протухание нерешённых черновиков, чей срок вышел.
func expire(ctx context.Context, tx *sql.Tx, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT body FROM drafts
		WHERE status IN (?, ?, ?) AND expires > 0 AND expires < ?`, Pending, Approved, Scheduled, now.Unix()+1)
	if err != nil {
		return err
	}
	var stale []*Draft
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			rows.Close()
			return err
		}
		if d, err := decode(body); err == nil && d.IsExpired(now) {
			stale = append(stale, d)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range stale {
		d.Status = Expired
		if err := save(ctx, tx, d); err != nil {
			return err
		}
	}
	return nil
}

func get(ctx context.Context, q store.Querier, id string) (*Draft, error) {
	var body string
	err := q.QueryRowContext(ctx, "SELECT body FROM drafts WHERE id = ?", id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &NotFound{ID: id}
	}
	if err != nil {
		return nil, err
	}
	d, err := decode(body)
	if err != nil {
		return nil, fmt.Errorf("черновик %s не читается: %w", id, err)
	}
	return d, nil
}

// FreshID — новый id, которого ещё нет в очереди (нужен до Create, когда
// под id заранее кладут вложения).
func (o *Outbox) FreshID() (string, error) {
	db, err := o.db()
	if err != nil {
		return "", err
	}
	for {
		id := NewID()
		var n int
		if err := db.Q().QueryRowContext(context.Background(), "SELECT count(*) FROM drafts WHERE id = ?", id).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return id, nil
		}
	}
}

// Create — новый черновик. Без opts.ID id выбирается сам (и при совпадении —
// заново); с заданным id, который уже занят, — отказ: под ним могли лежать
// вложения другого черновика.
func (o *Outbox) Create(opts CreateOpts) (*Draft, error) {
	now := time.Now()
	id, generated := opts.ID, opts.ID == ""
	if generated {
		id = NewID()
	}
	d := &Draft{
		ID:         id,
		Chat:       opts.Chat,
		Text:       opts.Text,
		ReplyTo:    opts.ReplyTo,
		CreatedAt:  audit.FormatTime(now),
		ExpiresAt:  audit.FormatTime(now.Add(time.Duration(opts.TTLMin) * time.Minute)),
		Status:     Pending,
		Origin:     cmpOr(opts.Origin, "agent"),
		Fmt:        cmpOr(opts.Fmt, "markdown"),
		Files:      opts.Files,
		Note:       opts.Note,
		Topic:      opts.Topic,
		TopicTitle: opts.TopicTitle,
		EditOf:     opts.EditOf,
		EditOld:    opts.EditOld,
		Poll:       opts.Poll,
		Vote:       opts.Vote,
		Bot:        opts.Bot,
	}
	if opts.SendAt != nil {
		at := audit.FormatTime(*opts.SendAt)
		d.Status = Scheduled
		d.SendAt = &at
		d.AddHistory("event", "scheduled", "by", "auto_send", "at", audit.FormatTime(now), "send_at", at)
	}
	db, err := o.db()
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	err = db.Write(ctx, func(tx *sql.Tx) error {
		if err := expire(ctx, tx, now); err != nil {
			return err
		}
		for {
			var n int
			if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM drafts WHERE id = ?", d.ID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				break
			}
			if !generated {
				return fmt.Errorf("%w: %s", ErrDuplicateID, d.ID)
			}
			d.ID = NewID()
		}
		if err := save(ctx, tx, d); err != nil {
			return err
		}
		// старые решённые — за пределами keep новейших
		_, err := tx.ExecContext(ctx, `DELETE FROM drafts WHERE status IN (?, ?, ?) AND sender_pid IS NULL AND seq <=
			(SELECT seq FROM drafts ORDER BY seq DESC LIMIT 1 OFFSET ?)`, Sent, Cancelled, Expired, keep)
		return err
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// List — черновики в порядке создания (status == "" — все).
func (o *Outbox) List(status string) ([]*Draft, error) {
	db, err := o.db()
	if err != nil {
		return nil, err
	}
	rows, err := db.Q().QueryContext(context.Background(), "SELECT body FROM drafts ORDER BY seq")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now()
	var out []*Draft
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		d, err := decode(body)
		if err != nil {
			continue // нечитаемая строка не должна прятать остальные
		}
		touch(d, now)
		if status == "" || d.Status == status {
			out = append(out, d)
		}
	}
	return out, rows.Err()
}

func (o *Outbox) Get(id string) (*Draft, error) {
	db, err := o.db()
	if err != nil {
		return nil, err
	}
	d, err := get(context.Background(), db.Q(), id)
	if err != nil {
		return nil, err
	}
	return touch(d, time.Now()), nil
}

// Update — изменить черновик одной транзакцией; mutate может отказать ошибкой.
func (o *Outbox) Update(id string, mutate func(*Draft) error) (*Draft, error) {
	db, err := o.db()
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	var found *Draft
	err = db.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now()
		if err := expire(ctx, tx, now); err != nil {
			return err
		}
		d, err := get(ctx, tx, id)
		if err != nil {
			return err
		}
		touch(d, now)
		if err := mutate(d); err != nil {
			return err
		}
		found = d
		return save(ctx, tx, d)
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// ClaimDue — забрать запланированные черновики, чьё время пришло: перевести
// их в approved (by auto_send) одной транзакцией, чтобы забрал ровно один
// процесс.
func (o *Outbox) ClaimDue(now time.Time) ([]*Draft, error) {
	db, err := o.db()
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	var due []*Draft
	err = db.Write(ctx, func(tx *sql.Tx) error {
		due = nil
		if err := expire(ctx, tx, now); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, "SELECT body FROM drafts WHERE status = ? AND send_at <= ? ORDER BY seq",
			Scheduled, now.Unix())
		if err != nil {
			return err
		}
		var list []*Draft
		for rows.Next() {
			var body string
			if err := rows.Scan(&body); err != nil {
				rows.Close()
				return err
			}
			if d, err := decode(body); err == nil {
				list = append(list, d)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, d := range list {
			if d.SendAt == nil {
				continue
			}
			// send_at в столбце — с точностью до секунды, а в черновике точнее
			at, err := time.Parse(time.RFC3339, *d.SendAt)
			if err != nil || now.Before(at) {
				continue
			}
			ts := audit.FormatTime(now)
			d.Status = Approved
			d.ApprovedAt = &ts
			d.AddHistory("event", "approved", "by", "auto_send", "at", ts)
			if err := save(ctx, tx, d); err != nil {
				return err
			}
			due = append(due, d)
		}
		return nil
	})
	return due, err
}

// NextDue — ближайшее время автоотправки (nil — ничего не запланировано).
func (o *Outbox) NextDue() (*time.Time, error) {
	db, err := o.db()
	if err != nil {
		return nil, err
	}
	var at sql.NullInt64
	if err := db.Q().QueryRowContext(context.Background(),
		"SELECT min(send_at) FROM drafts WHERE status = ? AND send_at IS NOT NULL", Scheduled).Scan(&at); err != nil {
		return nil, err
	}
	if !at.Valid {
		return nil, nil
	}
	t := time.Unix(at.Int64, 0)
	return &t, nil
}

// ── захват в отправку ────────────────────────────────────────────────────

// Статус approved держится до конца отправки, поэтому два пути (кнопка,
// автоотправка, досылка после перезапуска, tg_send_draft, tgw send-due без
// службы) без захвата отправили бы черновик дважды — 12.11.2026 так и
// вышло. Захват — отметка в строке черновика: кто отправляет (pid и когда
// взял). Взять можно, только если черновик approved и никто живой его не
// держит; проверка и отметка — одной транзакцией, поэтому взять его может
// только один путь любого процесса. Отметка умершего процесса (pid мёртв
// или занят процессом, запущенным позже захвата) — не держит.

// ErrDuplicateID — заданный id черновика уже занят.
var ErrDuplicateID = errors.New("черновик с таким id уже есть")

// ErrSending — черновик уже отправляет другой путь (этот или другой процесс).
var ErrSending = errors.New("черновик уже отправляется")

var (
	claimsMu sync.Mutex
	// claims — что держит этот процесс: id → когда взял. Отличает свой
	// живой захват от своего же оставшегося (снять отметку не вышло).
	claims = map[string]float64{}
)

// Claim — взятый в отправку черновик.
type Claim struct {
	o     *Outbox
	ID    string
	pid   int
	since float64
	// Retry — черновик уже брали в отправку раньше, а итога (отправлен,
	// записанный сбой) нет: прошлая попытка прервалась и могла дойти частью
	// или целиком. Отметка попытки пишется в том же захвате, до отправки.
	Retry bool
}

// pidAlive — жив ли процесс (подменяют тесты).
var pidAlive = lock.PIDAlive

// ClaimSend — взять одобренный черновик в отправку. Черновик не approved —
// (nil, черновик, nil): отправлять нечего, статус — в черновике. Его уже
// отправляет живой путь — ErrSending.
func (o *Outbox) ClaimSend(id string) (*Claim, *Draft, error) {
	db, err := o.db()
	if err != nil {
		return nil, nil, err
	}
	claimsMu.Lock()
	defer claimsMu.Unlock()
	ctx := context.Background()
	me := os.Getpid()
	var (
		claim *Claim
		draft *Draft
	)
	err = db.Write(ctx, func(tx *sql.Tx) error {
		claim, draft = nil, nil
		now := time.Now()
		if err := expire(ctx, tx, now); err != nil {
			return err
		}
		var (
			body     string
			pid      sql.NullInt64
			since    sql.NullFloat64
			attempts int
		)
		err := tx.QueryRowContext(ctx, "SELECT body, sender_pid, sender_since, send_attempts FROM drafts WHERE id = ?", id).
			Scan(&body, &pid, &since, &attempts)
		if errors.Is(err, sql.ErrNoRows) {
			return &NotFound{ID: id}
		}
		if err != nil {
			return err
		}
		d, err := decode(body)
		if err != nil {
			return fmt.Errorf("черновик %s не читается: %w", id, err)
		}
		draft = touch(d, now)
		if pid.Valid && holderAlive(id, int(pid.Int64), since.Float64) {
			return ErrSending
		}
		if d.Status != Approved {
			return nil
		}
		at := float64(now.UnixNano()) / 1e9
		if _, err := tx.ExecContext(ctx, "UPDATE drafts SET sender_pid = ?, sender_since = ?, send_attempts = send_attempts + 1 WHERE id = ?",
			me, at, id); err != nil {
			return err
		}
		claim = &Claim{o: o, ID: id, pid: me, since: at, Retry: attempts > 0 && d.SendError == nil}
		return nil
	})
	if err != nil {
		return nil, draft, err
	}
	if claim != nil {
		claims[id] = claim.since
	}
	return claim, draft, nil
}

// holderAlive — держит ли отметка: свой захват — пока он в claims, чужой —
// пока жив его процесс. Вызывать под claimsMu.
func holderAlive(id string, pid int, since float64) bool {
	if pid == os.Getpid() {
		got, ok := claims[id]
		return ok && got == since
	}
	return pidAlive(pid, since)
}

// Release — отпустить черновик. Не вышло снять отметку — она останется с
// нашим pid, но claims её уже не знает: и мы, и (после нашей смерти)
// другие сочтут её неживой.
func (c *Claim) Release() {
	claimsMu.Lock()
	if claims[c.ID] == c.since {
		delete(claims, c.ID)
	}
	claimsMu.Unlock()
	db, err := c.o.db()
	if err != nil {
		return
	}
	_, err = db.Q().ExecContext(context.Background(),
		"UPDATE drafts SET sender_pid = NULL, sender_since = NULL WHERE id = ? AND sender_pid = ? AND sender_since = ?",
		c.ID, c.pid, c.since)
	if err != nil {
		_ = audit.Log(auditPath(c.o.Dir), "send_release_failed", "draft_id", c.ID, "error", err.Error())
	}
}

func auditPath(dir string) string { return dir + string(os.PathSeparator) + "audit.jsonl" }
