package tgc

import (
	"context"
	"errors"
	"time"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"

	"tgagent/internal/config"
	"tgagent/internal/lock"
)

// Prompter — откуда взять телефон, код и пароль 2FA (терминал или GUI).
type Prompter interface {
	Phone(ctx context.Context) (string, error)
	Code(ctx context.Context) (string, error)
	Password(ctx context.Context) (string, error)
}

type authenticator struct{ p Prompter }

func (a authenticator) Phone(ctx context.Context) (string, error)    { return a.p.Phone(ctx) }
func (a authenticator) Password(ctx context.Context) (string, error) { return a.p.Password(ctx) }
func (a authenticator) Code(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
	return a.p.Code(ctx)
}
func (a authenticator) AcceptTermsOfService(context.Context, tg.HelpTermsOfService) error { return nil }
func (a authenticator) SignUp(context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, errors.New("аккаунта с этим номером нет — сначала зарегистрируйся в приложении Telegram")
}

// Login — вход и сохранение сессии (если уже вошли — просто возвращает аккаунт).
func Login(ctx context.Context, s *config.Settings, p Prompter) (*tg.User, error) {
	flock := lock.New(s.SessionPath, 45*time.Second)
	if err := flock.Acquire(ctx); err != nil {
		return nil, err
	}
	defer flock.Release()
	client, err := NewLoginClient(s)
	if err != nil {
		return nil, err
	}
	var me *tg.User
	err = client.Run(ctx, func(ctx context.Context) error {
		flow := auth.NewFlow(authenticator{p}, auth.SendCodeOptions{})
		if err := client.Auth().IfNecessary(ctx, flow); err != nil {
			return err
		}
		self, err := client.Self(ctx)
		me = self
		return err
	})
	return me, err
}

// Logout — отозвать сессию на стороне Telegram.
func Logout(ctx context.Context, s *config.Settings) error {
	return Run(ctx, s, Opts{NoAuth: true}, func(ctx context.Context, c *Conn) error {
		_, err := c.API.AuthLogOut(ctx)
		return err
	})
}
