package tgc

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gotd/td/clock"

	"tgagent/internal/config"
)

// Часы компьютера могут спешить или отставать: MTProto отвергает сообщения,
// если время клиента разошлось с серверным больше чем на ~30 с («msg_id too
// high»). Telethon поправляет сдвиг сам, gotd — нет. Поэтому узнаём время
// у Telegram (заголовок Date у api.telegram.org) и подводим свои часы.

type skewClock struct {
	offset time.Duration
}

func (c skewClock) Now() time.Time                      { return time.Now().Add(c.offset) }
func (c skewClock) Timer(d time.Duration) clock.Timer   { return clock.System.Timer(d) }
func (c skewClock) Ticker(d time.Duration) clock.Ticker { return clock.System.Ticker(d) }

var (
	skewMu      sync.Mutex
	skewValue   time.Duration
	skewChecked time.Time
)

// ClockOffset — насколько серверное время Telegram впереди нашего
// (отрицательное — наши часы спешат). Кэшируется на час.
func ClockOffset(s *config.Settings) time.Duration {
	skewMu.Lock()
	defer skewMu.Unlock()
	if !skewChecked.IsZero() && time.Since(skewChecked) < time.Hour {
		return skewValue
	}
	routes := []string{""}
	if s.BotProxy != "" {
		routes = append(routes, s.BotProxy)
	}
	for _, proxy := range routes {
		if off, ok := measure(proxy); ok {
			skewValue, skewChecked = off, time.Now()
			return skewValue
		}
	}
	return skewValue // не вышло — оставляем прошлое значение (или 0)
}

func measure(proxy string) (time.Duration, bool) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	if proxy != "" {
		if u, err := parseURL(proxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	client := &http.Client{Transport: tr, Timeout: 8 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, "https://api.telegram.org/", nil)
	before := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	resp.Body.Close()
	after := time.Now()
	server, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return 0, false
	}
	// Date округлён до секунды вниз: середина секунды точнее её начала
	server = server.Add(500 * time.Millisecond)
	local := before.Add(after.Sub(before) / 2)
	off := server.Sub(local)
	if off > -2*time.Second && off < 2*time.Second {
		off = 0 // в пределах точности заголовка — не трогаем
	}
	return off, true
}
