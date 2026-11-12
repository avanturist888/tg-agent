package tgc

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
)

func TestUpdStateSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "updates_state.json")
	u := openUpdState(path)
	if _, found, _ := u.GetState(ctx, 7); found {
		t.Fatal("пусто — состояния нет")
	}
	_ = u.SetState(ctx, 7, updates.State{Pts: 10, Qts: 1, Date: 100, Seq: 5})
	_ = u.SetPts(ctx, 7, 12)
	_ = u.SetChannelPts(ctx, 7, 555, 40)
	_ = u.SetChannelAccessHash(ctx, 7, 555, 999)
	u.flush()

	again := openUpdState(path)
	st, found, _ := again.GetState(ctx, 7)
	if !found || st.Pts != 12 || st.Seq != 5 {
		t.Fatalf("состояние после перезапуска: %+v %v", st, found)
	}
	if pts, ok, _ := again.GetChannelPts(ctx, 7, 555); !ok || pts != 40 {
		t.Fatalf("pts канала: %d %v", pts, ok)
	}
	if h, ok, _ := again.GetChannelAccessHash(ctx, 7, 555); !ok || h != 999 {
		t.Fatalf("access_hash канала: %d %v", h, ok)
	}
	n := 0
	_ = again.ForEachChannels(ctx, 7, func(context.Context, int64, int) error { n++; return nil })
	if n != 1 {
		t.Fatalf("каналов: %d", n)
	}
	// другой аккаунт — чужое состояние не отдаём
	if _, found, _ := again.GetState(ctx, 8); found {
		t.Fatal("состояние другого аккаунта")
	}
	again.reset()
	if _, found, _ := openUpdState(path).GetState(ctx, 7); found {
		t.Fatal("после выхода из аккаунта состояние забыто")
	}
}

// Канал, известный только из списка диалогов (peers.json), менеджер
// обновлений получает без лишнего getDifference; чужому аккаунту — нет.
func TestUpdStateChannelHashFromPeers(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	u := openUpdState(filepath.Join(dir, "updates_state.json"))
	if _, ok, _ := u.GetChannelAccessHash(ctx, 7, 555); ok {
		t.Fatal("без кэша пиров хеша нет")
	}
	p := LoadPeers(filepath.Join(dir, "peers.json"))
	ch := &tg.Channel{ID: 555}
	ch.SetAccessHash(321)
	p.Remember([]tg.UserClass{&tg.User{ID: 7, Self: true}}, []tg.ChatClass{ch})
	u.usePeers(p)
	if h, ok, _ := u.GetChannelAccessHash(ctx, 7, 555); !ok || h != 321 {
		t.Fatalf("хеш из кэша пиров: %d %v", h, ok)
	}
	if _, ok, _ := u.GetChannelAccessHash(ctx, 8, 555); ok {
		t.Fatal("кэш пиров другого аккаунта")
	}
	// свой хеш менеджера главнее
	_ = u.SetChannelAccessHash(ctx, 7, 555, 999)
	if h, _, _ := u.GetChannelAccessHash(ctx, 7, 555); h != 999 {
		t.Fatalf("хеш менеджера: %d", h)
	}
}
