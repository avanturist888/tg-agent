package tgc

// Отметка обновлений Telegram на диске (kv updates_state и таблица
// update_channels в data/state.db).
//
// Менеджер обновлений gotd догоняет пропущенное запросом «что изменилось с
// pts N». С отметкой только в памяти он догонял лишь обрывы сети внутри
// одного запуска, а после перезапуска службы начинал с текущего момента:
// правки и удаления за время простоя пропадали (новые сообщения ловит
// сверка лент). С отметкой на диске служба после старта сама получает
// пропущенное обычными событиями — насколько далеко назад, решает Telegram
// (слишком долгий простой — updates.differenceTooLong, остаётся сверка).
//
// Запись — не на каждый pts, а раз в несколько секунд и только того, что
// поменялось: при падении служба повторит последние события, а это
// безвредно (ленты не дублируются, правку с той же датой отсекает
// editTracker). Пишет только служба.

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/gotd/td/telegram/updates"

	"tgagent/internal/store"
)

const updStateFlush = 3 * time.Second

type updState struct {
	dir  string
	mu   sync.Mutex
	user int64
	// state — общая отметка (nil — ещё не было)
	state *updates.State
	pts   map[int64]int   // канал → pts
	hash  map[int64]int64 // канал → access_hash
	// что записать: общая отметка, каналы; wipe — сначала стереть все
	// каналы (сменился аккаунт)
	dirtyState bool
	dirtyCh    map[int64]bool
	wipe       bool
	// peers — запасной источник access_hash каналов (таблицы peer_*): канал,
	// известный только из списка диалогов, иначе стоил бы менеджеру лишнего
	// getDifference на первом же своём событии
	peers *PeerCache
}

// usePeers — подключить кэш пиров текущего соединения.
func (u *updState) usePeers(p *PeerCache) {
	u.mu.Lock()
	u.peers = p
	u.mu.Unlock()
}

// updHead — общая отметка в kv.
type updHead struct {
	User  int64          `json:"user"`
	State *updates.State `json:"state"`
}

func openUpdState(dir string) *updState {
	u := &updState{dir: dir, pts: map[int64]int{}, hash: map[int64]int64{}, dirtyCh: map[int64]bool{}}
	db, err := store.Open(dir)
	if err != nil {
		return u
	}
	ctx := context.Background()
	var head updHead
	if ok, err := store.GetKV(ctx, db.Q(), store.KeyUpdatesState, &head); !ok || err != nil {
		return u
	}
	u.user, u.state = head.User, head.State
	rows, err := db.Q().QueryContext(ctx, "SELECT channel_id, pts, access_hash FROM update_channels")
	if err != nil {
		return u
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id   int64
			pts  sql.NullInt64
			hash sql.NullInt64
		)
		if rows.Scan(&id, &pts, &hash) != nil {
			continue
		}
		if pts.Valid {
			u.pts[id] = int(pts.Int64)
		}
		if hash.Valid {
			u.hash[id] = hash.Int64
		}
	}
	return u
}

// forUser — отметка принадлежит аккаунту: другой аккаунт начинает с нуля.
func (u *updState) forUser(userID int64) {
	if u.user != userID {
		u.user, u.state = userID, nil
		u.pts, u.hash, u.dirtyCh = map[int64]int{}, map[int64]int64{}, map[int64]bool{}
		u.wipe, u.dirtyState = true, true
	}
}

// reset — выход из аккаунта: отметку забыть сразу.
func (u *updState) reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.user, u.state = 0, nil
	u.pts, u.hash, u.dirtyCh = map[int64]int{}, map[int64]int64{}, map[int64]bool{}
	u.wipe, u.dirtyState = false, false
	db, err := store.Open(u.dir)
	if err != nil {
		return
	}
	ctx := context.Background()
	_ = db.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM update_channels"); err != nil {
			return err
		}
		return store.DelKV(ctx, tx, store.KeyUpdatesState)
	})
}

func (u *updState) flush() {
	u.mu.Lock()
	if !u.dirtyState && len(u.dirtyCh) == 0 && !u.wipe {
		u.mu.Unlock()
		return
	}
	head := updHead{User: u.user}
	if u.state != nil {
		st := *u.state
		head.State = &st
	}
	wipe := u.wipe
	type ch struct {
		id        int64
		pts, hash any // nil — не знаем (NULL)
	}
	var chans []ch
	for id := range u.dirtyCh {
		c := ch{id: id}
		if v, ok := u.pts[id]; ok {
			c.pts = v
		}
		if v, ok := u.hash[id]; ok {
			c.hash = v
		}
		chans = append(chans, c)
	}
	u.dirtyState, u.wipe, u.dirtyCh = false, false, map[int64]bool{}
	u.mu.Unlock()

	db, err := store.Open(u.dir)
	if err == nil {
		ctx := context.Background()
		err = db.Write(ctx, func(tx *sql.Tx) error {
			if wipe {
				if _, err := tx.ExecContext(ctx, "DELETE FROM update_channels"); err != nil {
					return err
				}
			}
			for _, c := range chans {
				if _, err := tx.ExecContext(ctx, `INSERT INTO update_channels(channel_id, pts, access_hash) VALUES(?, ?, ?)
					ON CONFLICT(channel_id) DO UPDATE SET pts = excluded.pts, access_hash = excluded.access_hash`,
					c.id, c.pts, c.hash); err != nil {
					return err
				}
			}
			return store.PutKV(ctx, tx, store.KeyUpdatesState, head)
		})
	}
	if err != nil {
		// не записалось — повторить со следующим flush
		u.mu.Lock()
		u.dirtyState = true
		u.wipe = u.wipe || wipe
		for _, c := range chans {
			u.dirtyCh[c.id] = true
		}
		u.mu.Unlock()
	}
}

func (u *updState) keep(ctx context.Context) {
	for ctx.Err() == nil {
		sleep(ctx, updStateFlush)
		u.flush()
	}
	u.flush()
}

func (u *updState) edit(userID int64, fn func(st *updates.State)) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.forUser(userID)
	if u.state == nil {
		u.state = &updates.State{}
	}
	fn(u.state)
	u.dirtyState = true
	return nil
}

// ── updates.StateStorage ─────────────────────────────────────────────────

func (u *updState) GetState(_ context.Context, userID int64) (updates.State, bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.user != userID || u.state == nil {
		return updates.State{}, false, nil
	}
	return *u.state, true, nil
}

func (u *updState) SetState(_ context.Context, userID int64, state updates.State) error {
	return u.edit(userID, func(st *updates.State) { *st = state })
}

func (u *updState) SetPts(_ context.Context, userID int64, pts int) error {
	return u.edit(userID, func(st *updates.State) { st.Pts = pts })
}

func (u *updState) SetQts(_ context.Context, userID int64, qts int) error {
	return u.edit(userID, func(st *updates.State) { st.Qts = qts })
}

func (u *updState) SetDate(_ context.Context, userID int64, date int) error {
	return u.edit(userID, func(st *updates.State) { st.Date = date })
}

func (u *updState) SetSeq(_ context.Context, userID int64, seq int) error {
	return u.edit(userID, func(st *updates.State) { st.Seq = seq })
}

func (u *updState) SetDateSeq(_ context.Context, userID int64, date, seq int) error {
	return u.edit(userID, func(st *updates.State) { st.Date, st.Seq = date, seq })
}

func (u *updState) GetChannelPts(_ context.Context, userID, channelID int64) (int, bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.user != userID {
		return 0, false, nil
	}
	pts, ok := u.pts[channelID]
	return pts, ok, nil
}

func (u *updState) SetChannelPts(_ context.Context, userID, channelID int64, pts int) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.forUser(userID)
	u.pts[channelID] = pts
	u.dirtyCh[channelID] = true
	return nil
}

func (u *updState) ForEachChannels(ctx context.Context, userID int64, f func(ctx context.Context, channelID int64, pts int) error) error {
	u.mu.Lock()
	if u.user != userID {
		u.mu.Unlock()
		return nil
	}
	chans := make(map[int64]int, len(u.pts))
	for id, pts := range u.pts {
		chans[id] = pts
	}
	u.mu.Unlock()
	for id, pts := range chans {
		if err := f(ctx, id, pts); err != nil {
			return err
		}
	}
	return nil
}

// ── updates.ChannelAccessHasher ──────────────────────────────────────────

func (u *updState) SetChannelAccessHash(_ context.Context, userID, channelID, accessHash int64) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.forUser(userID)
	if h, ok := u.hash[channelID]; !ok || h != accessHash {
		u.hash[channelID] = accessHash
		u.dirtyCh[channelID] = true
	}
	return nil
}

func (u *updState) GetChannelAccessHash(_ context.Context, userID, channelID int64) (int64, bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.user == userID {
		if h, ok := u.hash[channelID]; ok {
			return h, true, nil
		}
	}
	if p := u.peers; p != nil && p.SelfID() == userID {
		if h, ok := p.channel(channelID); ok {
			return h, true, nil
		}
	}
	return 0, false, nil
}

var (
	_ updates.StateStorage        = (*updState)(nil)
	_ updates.ChannelAccessHasher = (*updState)(nil)
)
