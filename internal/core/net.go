package core

// Сети, кроме Telegram (MAX, WhatsApp): у каждой свой клиент в службе
// профиля, а белый список, черновики, карточки и подписки общие. Всё, что
// ходит в саму сеть, — за этим интерфейсом: в общем коде одна развилка
// «Telegram или другая сеть», а не по ветке на каждую.

import (
	"context"
	"errors"
	"fmt"

	"tgagent/internal/config"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/tgc"
)

type network interface {
	name() string // для текстов агенту: «В MAX тем нет»
	run(ctx context.Context, loader func() (*config.Settings, error))
	readChat(ctx context.Context, s *config.Settings, rule config.ChatRule, o ReadOpts, limit int) ([]*omap.Map, error)
	deliver(ctx context.Context, s *config.Settings, rule config.ChatRule, d *outbox.Draft) (*omap.Map, error)
	editOld(ctx context.Context, s *config.Settings, rule config.ChatRule, messageID int64) (string, error)
	chatStatus(ctx context.Context, s *config.Settings) (map[string]*omap.Map, error)
	downloadFile(ctx context.Context, s *config.Settings, rule config.ChatRule, messageID int64) (*omap.Map, error)
	viewMedia(ctx context.Context, s *config.Settings, rule config.ChatRule, messageID int64) (*omap.Map, *tgc.Image, error)
	react(ctx context.Context, rule config.ChatRule, messageID int64, emoji string) ([]*omap.Map, error)
	transcribe(ctx context.Context, s *config.Settings, rule config.ChatRule, messageID int64, engine string) (*omap.Map, error)
	listFolders(ctx context.Context, s *config.Settings) (*omap.Map, error)
	folderChats(ctx context.Context, s *config.Settings, folder string) (*omap.Map, error)
	folderSample(ctx context.Context, s *config.Settings, r *accessRequest, folder string) error
	setMembers(ctx context.Context, s *config.Settings) (map[string]config.SetState, error)
	chatInfo(ctx context.Context, chatID int64) (tgc.DialogRow, error)
	ruleChatID(rule config.ChatRule) (int64, error)
	catchup(ctx context.Context, s *config.Settings)
	dialogs(ctx context.Context) ([]tgc.DialogRow, error)
	selfLabel() string
}

// netOf — сеть профиля; nil — Telegram.
func netOf(s *config.Settings) network {
	switch {
	case s.IsMAX():
		return maxNet{}
	case s.IsWA():
		return waNet{}
	}
	return nil
}

// NetName — название сети для текстов («MAX», «WhatsApp», «Telegram»).
func NetName(network string) string {
	switch network {
	case config.NetMAX:
		return "MAX"
	case config.NetWA:
		return "WhatsApp"
	}
	return "Telegram"
}

// RunNetwork — клиент сети профиля в службе (MAX, WhatsApp); false —
// профиль Telegram, им занимается tgc.Serve.
func RunNetwork(ctx context.Context, s *config.Settings, loader func() (*config.Settings, error)) bool {
	n := netOf(s)
	if n == nil {
		return false
	}
	n.run(ctx, loader)
	return true
}

// NetworkDialogs — чаты аккаунта сети для tg allow --match и op dialogs.
func NetworkDialogs(ctx context.Context, s *config.Settings) ([]tgc.DialogRow, bool, error) {
	n := netOf(s)
	if n == nil {
		return nil, false, nil
	}
	rows, err := n.dialogs(ctx)
	return rows, true, err
}

// NetworkAccount — чей вход у клиента сети (для doctor). *Bad — входа нет.
func NetworkAccount(ctx context.Context, s *config.Settings) (string, error) {
	switch {
	case s.IsMAX():
		st, err := MaxState(ctx)
		if errors.Is(err, ErrNoMax) {
			return "", &Bad{Msg: err.Error()}
		}
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s (id %d)", st.Name, st.UserID), nil
	case s.IsWA():
		st, err := WAState(ctx)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s (+%s)", st.Name, st.Phone), nil
	}
	return "", errors.New("профиль Telegram")
}

// maxNet — MAX (max.go, max_media.go).
type maxNet struct{}

func (maxNet) name() string { return "MAX" }
func (maxNet) run(ctx context.Context, loader func() (*config.Settings, error)) {
	RunMax(ctx, loader)
}
func (maxNet) readChat(ctx context.Context, s *config.Settings, rule config.ChatRule, o ReadOpts, limit int) ([]*omap.Map, error) {
	return maxReadChat(ctx, s, rule, o, limit)
}
func (maxNet) deliver(ctx context.Context, s *config.Settings, rule config.ChatRule, d *outbox.Draft) (*omap.Map, error) {
	return maxDeliver(ctx, s, rule, d)
}
func (maxNet) editOld(ctx context.Context, s *config.Settings, rule config.ChatRule, id int64) (string, error) {
	return maxEditOld(ctx, s, rule, id)
}
func (maxNet) chatStatus(ctx context.Context, s *config.Settings) (map[string]*omap.Map, error) {
	return maxChatStatus(ctx, s)
}
func (maxNet) downloadFile(ctx context.Context, s *config.Settings, rule config.ChatRule, id int64) (*omap.Map, error) {
	return maxDownloadFile(ctx, s, rule, id)
}
func (maxNet) viewMedia(ctx context.Context, s *config.Settings, rule config.ChatRule, id int64) (*omap.Map, *tgc.Image, error) {
	return maxViewMedia(ctx, s, rule, id)
}
func (maxNet) react(ctx context.Context, rule config.ChatRule, id int64, emoji string) ([]*omap.Map, error) {
	return maxReact(ctx, rule, id, emoji)
}
func (maxNet) transcribe(ctx context.Context, s *config.Settings, rule config.ChatRule, id int64, engine string) (*omap.Map, error) {
	switch engine {
	case "", "auto", "gigaam", "max":
	default:
		return nil, &Bad{Msg: "engine в MAX: auto, gigaam или max (расшифровка самого MAX)."}
	}
	return maxTranscribeMessage(ctx, s, rule, id, engine)
}
func (maxNet) listFolders(ctx context.Context, s *config.Settings) (*omap.Map, error) {
	return maxListFolders(ctx, s)
}
func (maxNet) folderChats(ctx context.Context, s *config.Settings, folder string) (*omap.Map, error) {
	return maxFolderChats(ctx, s, folder)
}
func (maxNet) folderSample(ctx context.Context, s *config.Settings, r *accessRequest, folder string) error {
	return maxFolderSample(ctx, s, r, folder)
}
func (maxNet) setMembers(ctx context.Context, s *config.Settings) (map[string]config.SetState, error) {
	return maxSetMembers(ctx, s)
}
func (maxNet) chatInfo(ctx context.Context, id int64) (tgc.DialogRow, error) {
	return maxChatInfo(ctx, id)
}
func (maxNet) ruleChatID(rule config.ChatRule) (int64, error)  { return maxChatID(rule) }
func (maxNet) catchup(ctx context.Context, s *config.Settings) { maxCatchup(ctx, s) }
func (maxNet) dialogs(ctx context.Context) ([]tgc.DialogRow, error) {
	chats, err := MaxDialogs(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]tgc.DialogRow, len(chats))
	for i, c := range chats {
		kind := map[string]string{"DIALOG": "user", "CHAT": "group", "CHANNEL": "channel"}[c.Kind]
		rows[i] = tgc.DialogRow{ID: c.ID, Title: c.Title, Kind: kind, Unread: c.Unread}
	}
	return rows, nil
}
func (maxNet) selfLabel() string { return maxSelfLabel() }
