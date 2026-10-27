// Package upstream следит за тем, от чего зависит клиент MAX: веб-клиент MAX
// (его версию клиент называет при входе, адрес WebSocket — оттуда же),
// каталог сборок Android-клиента и репозиторий PyMax, с которого переносится
// протокол. Здесь только проверки и сравнение с прошлым; расписание, бот и
// закрепление карточек — core/upstream.go.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Values — что видно у источника сейчас: ключ → значение.
type Values map[string]string

// Diff — изменившееся значение с подписью для карточки.
type Diff struct {
	Key, Label, Old, New string
}

// Source — один источник. Check получает прошлые значения (nil — первая
// проверка) и возвращает новые и пояснения к изменению (список коммитов…).
type Source struct {
	ID, Title, URL string
	// Labels — что показывать владельцу; ключи без подписи хранятся молча
	// (полный sha, список версий), их изменение карточку не шлёт.
	Labels []Label
	Check  func(ctx context.Context, c *Client, prev Values) (Values, []string, error)
}

type Label struct{ Key, Text string }

// Result — итог проверки одного источника; состояние меняет только Apply.
type Result struct {
	Source  *Source
	Prev    Values
	Cur     Values
	Diffs   []Diff
	Details []string
	Err     error
	First   bool // источника ещё не было в состоянии: запомнить, не сообщать
}

// Changed — есть что сообщить владельцу.
func (r Result) Changed() bool { return r.Err == nil && !r.First && len(r.Diffs) > 0 }

// State — data/upstream.json.
type State struct {
	Sources map[string]*SourceState `json:"sources"`
}

type SourceState struct {
	Values  Values    `json:"values"`
	Checked time.Time `json:"checked"`
	Changed time.Time `json:"changed,omitzero"`
	Error   string    `json:"error,omitempty"`
	Fails   int       `json:"fails,omitempty"` // проверок подряд с ошибкой
}

// Load — состояние с диска; нет файла — пустое.
func Load(path string) (*State, error) {
	st := &State{Sources: map[string]*SourceState{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if st.Sources == nil {
		st.Sources = map[string]*SourceState{}
	}
	return st, nil
}

func (st *State) Save(path string) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Check — проверить источники; состояние не трогает.
func Check(ctx context.Context, c *Client, st *State, sources []*Source) []Result {
	out := make([]Result, 0, len(sources))
	for _, src := range sources {
		r := Result{Source: src}
		if ss := st.Sources[src.ID]; ss != nil && ss.Values != nil {
			r.Prev = ss.Values
		} else {
			r.First = true
		}
		r.Cur, r.Details, r.Err = src.Check(ctx, c, r.Prev)
		if r.Err == nil && !r.First {
			for _, l := range src.Labels {
				if old, cur := r.Prev[l.Key], r.Cur[l.Key]; old != cur {
					r.Diffs = append(r.Diffs, Diff{Key: l.Key, Label: l.Text, Old: old, New: cur})
				}
			}
		}
		out = append(out, r)
	}
	return out
}

// Apply — записать итог в состояние. Изменение, о котором не удалось
// сообщить, не применяют: следующая проверка увидит его снова.
func (st *State) Apply(r Result, now time.Time) {
	ss := st.Sources[r.Source.ID]
	if ss == nil {
		ss = &SourceState{}
		st.Sources[r.Source.ID] = ss
	}
	ss.Checked = now
	if r.Err != nil {
		ss.Error = r.Err.Error()
		ss.Fails++
		return
	}
	ss.Error, ss.Fails = "", 0
	if len(r.Diffs) > 0 {
		ss.Changed = now
	}
	ss.Values = r.Cur
}

// Client — HTTP для проверок: без прокси Telegram, с ограничением размера.
type Client struct {
	HTTP *http.Client
	// адреса источников; тесты подменяют их на httptest
	MaxWeb, Hashes, GitHub string
}

// NewClient — клиент с настоящими адресами.
func NewClient() *Client {
	return &Client{
		HTTP:   &http.Client{Timeout: time.Minute},
		MaxWeb: "https://web.max.ru",
		Hashes: "https://hashes.pymax.org/versions.json",
		GitHub: "https://api.github.com",
	}
}

const maxBody = 16 << 20 // самый большой кусок веб-клиента — около 2,5 МБ

func (c *Client) get(ctx context.Context, url string, hdr map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; tg-agent upstream check)")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{URL: url, Status: resp.StatusCode}
	}
	return body, nil
}

type HTTPError struct {
	URL    string
	Status int
}

func (e *HTTPError) Error() string { return fmt.Sprintf("%s: HTTP %d", e.URL, e.Status) }
