package tgc

// Отметка обновлений Telegram на диске (data/updates_state.json).
//
// Менеджер обновлений gotd догоняет пропущенное запросом «что изменилось с
// pts N». С отметкой только в памяти он догонял лишь обрывы сети внутри
// одного запуска, а после перезапуска службы начинал с текущего момента:
// правки и удаления за время простоя пропадали (новые сообщения ловит
// сверка лент). С отметкой на диске служба после старта сама получает
// пропущенное обычными событиями — насколько далеко назад, решает Telegram
// (слишком долгий простой — updates.differenceTooLong, остаётся сверка).
//
// Запись — не на каждый pts, а раз в несколько секунд: при падении служба
// повторит последние события, а это безвредно (ленты не дублируются,
// правку с той же датой отсекает editTracker).

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gotd/td/telegram/updates"
)

const updStateFlush = 3 * time.Second

type updState struct {
	path  string
	mu    sync.Mutex
	dirty bool
	data  updStateFile
}

type updStateFile struct {
	User     int64            `json:"user"`
	State    *updates.State   `json:"state,omitempty"`
	Channels map[string]int   `json:"channels"`      // id канала → pts
	Hashes   map[string]int64 `json:"access_hashes"` // id канала → access_hash
}

func openUpdState(path string) *updState {
	u := &updState{path: path}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &u.data)
	}
	if u.data.Channels == nil {
		u.data.Channels = map[string]int{}
	}
	if u.data.Hashes == nil {
		u.data.Hashes = map[string]int64{}
	}
	return u
}

func chKey(id int64) string { return strconv.FormatInt(id, 10) }

// forUser — данные этого аккаунта; другой аккаунт (вход заново) — с чистого листа.
func (u *updState) forUser(userID int64) {
	if u.data.User != userID {
		u.data = updStateFile{User: userID, Channels: map[string]int{}, Hashes: map[string]int64{}}
		u.dirty = true
	}
}

// reset — забыть всё (выход из аккаунта).
func (u *updState) reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.data = updStateFile{Channels: map[string]int{}, Hashes: map[string]int64{}}
	u.dirty = false
	_ = os.Remove(u.path)
}

func (u *updState) flush() {
	u.mu.Lock()
	if !u.dirty {
		u.mu.Unlock()
		return
	}
	raw, err := json.Marshal(u.data)
	u.dirty = false
	u.mu.Unlock()
	if err != nil {
		return
	}
	tmp := u.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, u.path)
	}
}

// keep — сбрасывать на диск, пока служба жива, и в конце.
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
	if u.data.State == nil {
		u.data.State = &updates.State{}
	}
	fn(u.data.State)
	u.dirty = true
	return nil
}

// ── updates.StateStorage ─────────────────────────────────────────────────

func (u *updState) GetState(_ context.Context, userID int64) (updates.State, bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.data.User != userID || u.data.State == nil {
		return updates.State{}, false, nil
	}
	return *u.data.State, true, nil
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
	if u.data.User != userID {
		return 0, false, nil
	}
	pts, ok := u.data.Channels[chKey(channelID)]
	return pts, ok, nil
}

func (u *updState) SetChannelPts(_ context.Context, userID, channelID int64, pts int) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.forUser(userID)
	u.data.Channels[chKey(channelID)] = pts
	u.dirty = true
	return nil
}

func (u *updState) ForEachChannels(ctx context.Context, userID int64, f func(ctx context.Context, channelID int64, pts int) error) error {
	u.mu.Lock()
	if u.data.User != userID {
		u.mu.Unlock()
		return nil
	}
	chans := make(map[int64]int, len(u.data.Channels))
	for k, pts := range u.data.Channels {
		if id, err := strconv.ParseInt(k, 10, 64); err == nil {
			chans[id] = pts
		}
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
	if u.data.Hashes[chKey(channelID)] != accessHash {
		u.data.Hashes[chKey(channelID)] = accessHash
		u.dirty = true
	}
	return nil
}

func (u *updState) GetChannelAccessHash(_ context.Context, userID, channelID int64) (int64, bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.data.User != userID {
		return 0, false, nil
	}
	h, ok := u.data.Hashes[chKey(channelID)]
	return h, ok, nil
}

var (
	_ updates.StateStorage        = (*updState)(nil)
	_ updates.ChannelAccessHasher = (*updState)(nil)
)
