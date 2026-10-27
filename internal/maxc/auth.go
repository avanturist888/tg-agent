package maxc

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// UserAgent — как клиент представляется серверу (SESSION_INIT). Порядок
// полей — как у веб-клиента; тот же набор хранится в сессии: после входа
// устройство должно выглядеть прежним.
type UserAgent struct {
	DeviceType      string `json:"deviceType"`
	Locale          string `json:"locale"`
	DeviceLocale    string `json:"deviceLocale"`
	OSVersion       string `json:"osVersion"`
	DeviceName      string `json:"deviceName"`
	HeaderUserAgent string `json:"headerUserAgent"`
	AppVersion      string `json:"appVersion"`
	Screen          string `json:"screen"`
	Timezone        string `json:"timezone"`
}

// FallbackWebVersion — версия веб-клиента, если свежую узнать не удалось
// (сервер принимает и устаревшие: PyMax входит с 26.8.4).
const FallbackWebVersion = "26.10.7"

// WebUserAgent — веб-клиент в Chrome под Windows.
func WebUserAgent(appVersion string) UserAgent {
	if appVersion == "" {
		appVersion = FallbackWebVersion
	}
	return UserAgent{
		DeviceType: "WEB", Locale: "ru", DeviceLocale: "ru", OSVersion: "Windows",
		DeviceName: "Chrome",
		HeaderUserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
			"(KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36",
		AppVersion: appVersion, Screen: "1080x1920 1.0x", Timezone: "Europe/Moscow",
	}
}

// Handshake — SESSION_INIT: первое, что ждёт сервер после подключения.
func (c *Conn) Handshake(ctx context.Context, ua UserAgent, deviceID string) error {
	return c.Invoke(ctx, OpSessionInit, map[string]any{"userAgent": ua, "deviceId": deviceID}, nil)
}

// ── вход по токену ───────────────────────────────────────────────────────

// ErrTokenRevoked — сервер не принял токен: сессию завершили (на телефоне
// или «выйти со всех устройств»). Нужен новый вход по QR — сам клиент
// его не начинает.
var ErrTokenRevoked = errors.New("maxc: сессия MAX завершена — нужен вход по QR")

type Name struct {
	Name      string `json:"name"`
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
	Type      string `json:"type"`
}

type Contact struct {
	ID    int64  `json:"id"`
	Names []Name `json:"names"`
	Phone int64  `json:"phone"`
}

// DisplayName — имя для людей.
func (c Contact) DisplayName() string {
	for _, n := range c.Names {
		if n.Name != "" {
			return n.Name
		}
		if s := n.FirstName + " " + n.LastName; s != " " {
			return s
		}
	}
	return fmt.Sprint(c.ID)
}

type Profile struct {
	Contact Contact `json:"contact"`
}

type Chat struct {
	ID            int64    `json:"id"`
	Type          string   `json:"type"` // DIALOG, CHAT, CHANNEL
	Status        string   `json:"status"`
	Owner         int64    `json:"owner"`
	Title         string   `json:"title"`
	LastMessage   *Message `json:"lastMessage"`
	LastEventTime int64    `json:"lastEventTime"`
}

// LoginResult — ответ на LOGIN: профиль, первые чаты и контакты.
type LoginResult struct {
	Profile  Profile   `json:"profile"`
	Chats    []Chat    `json:"chats"`
	Contacts []Contact `json:"contacts"`
}

// Login — вход по токену сессии. Синхронизацию просим полную (-1): кэша у
// шлюза нет.
func (c *Conn) Login(ctx context.Context, token string) (*LoginResult, error) {
	var r LoginResult
	err := c.Invoke(ctx, OpLogin, map[string]any{
		"token": token, "chatsCount": 40, "interactive": true,
		"chatsSync": -1, "contactsSync": -1, "presenceSync": -1, "draftsSync": -1,
	}, &r)
	var e *Error
	if errors.As(err, &e) && (e.Code == "FAIL_LOGIN_TOKEN" || e.Code == "FAIL_LOGOUT_ALL" ||
		e.Message == "FAIL_LOGIN_TOKEN" || e.Message == "FAIL_LOGOUT_ALL") {
		return nil, fmt.Errorf("%w (%s)", ErrTokenRevoked, e.Code)
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ── вход по QR ───────────────────────────────────────────────────────────

// QR — код для входа: ссылку показать QR-кодом, отсканировать в
// приложении MAX на телефоне.
type QR struct {
	TrackID  string `json:"trackId"`
	Link     string `json:"qrLink"`
	Expires  int64  `json:"expiresAt"`       // мс Unix
	Interval int64  `json:"pollingInterval"` // мс
}

func (q QR) ExpiresAt() time.Time { return time.UnixMilli(q.Expires) }

// ErrQRExpired — QR не отсканировали вовремя (живёт около двух минут).
var ErrQRExpired = errors.New("maxc: QR-код истёк")

// ErrPasswordRequired — у аккаунта облачный пароль, а спросить его не у кого.
var ErrPasswordRequired = errors.New("maxc: нужен облачный пароль аккаунта")

// LoginQR — показать QR (show), дождаться подтверждения на телефоне и, если
// у аккаунта облачный пароль, спросить его (password; hint — подсказка
// пароля, prev — почему не принят прошлый, nil при первом вопросе).
// Возвращает токен сессии.
func (c *Conn) LoginQR(ctx context.Context, show func(QR) error,
	password func(hint string, prev error) (string, error)) (token string, err error) {
	var q QR
	if err := c.Invoke(ctx, OpGetQR, nil, &q); err != nil {
		return "", err
	}
	if q.TrackID == "" || q.Link == "" {
		return "", errors.New("maxc: сервер не выдал QR")
	}
	if err := show(q); err != nil {
		return "", err
	}
	interval := time.Duration(q.Interval) * time.Millisecond // сервер просит 5 с
	switch {
	case interval <= 0:
		interval = 5 * time.Second
	case interval < 100*time.Millisecond:
		interval = 100 * time.Millisecond
	}
	for {
		if time.Now().After(q.ExpiresAt()) {
			return "", ErrQRExpired
		}
		var st struct {
			Status struct {
				LoginAvailable bool  `json:"loginAvailable"`
				Expires        int64 `json:"expiresAt"`
			} `json:"status"`
		}
		if err := c.Invoke(ctx, OpQRStatus, map[string]any{"trackId": q.TrackID}, &st); err != nil {
			return "", err
		}
		if st.Status.LoginAvailable {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
	}
	var r tokenReply
	if err := c.Invoke(ctx, OpLoginByQR, map[string]any{"trackId": q.TrackID}, &r); err != nil {
		return "", err
	}
	if t := r.TokenAttrs.Login.Token; t != "" {
		return t, nil
	}
	ch := r.PasswordChallenge
	if ch == nil || ch.TrackID == "" {
		return "", errors.New("maxc: вход подтверждён, но сервер не выдал ни токена, ни запроса пароля")
	}
	if password == nil {
		return "", ErrPasswordRequired
	}
	var last error
	for range passwordAttempts {
		pw, err := password(ch.Hint, last)
		if err != nil {
			return "", err
		}
		if pw == "" {
			return "", errors.New("maxc: пароль не введён")
		}
		var pr tokenReply
		err = c.Invoke(ctx, OpCheckPassword, map[string]any{"trackId": ch.TrackID, "password": pw}, &pr)
		var e *Error
		if errors.As(err, &e) {
			last = err // неверный пароль — спросить ещё раз
			continue
		}
		if err != nil {
			return "", err
		}
		if pr.TokenAttrs.Login.Token == "" {
			return "", errors.New("maxc: пароль принят, но токена в ответе нет")
		}
		return pr.TokenAttrs.Login.Token, nil
	}
	return "", last
}

// passwordAttempts — сколько раз спросить пароль за один QR (как
// password_max_attempts у PyMax).
const passwordAttempts = 3

type tokenReply struct {
	TokenAttrs struct {
		Login struct {
			Token string `json:"token"`
		} `json:"LOGIN"`
	} `json:"tokenAttrs"`
	PasswordChallenge *struct {
		TrackID string `json:"trackId"`
		Hint    string `json:"hint"`
	} `json:"passwordChallenge"`
}
