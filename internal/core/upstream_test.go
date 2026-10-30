package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"tgagent/internal/bot"
	"tgagent/internal/config"
	"tgagent/internal/upstream"
)

// fakeUpstream — источник с подставным значением и бот, который записывает вызовы.
type fakeUpstream struct {
	mu       sync.Mutex
	value    string
	err      error
	calls    []string // method + текст/id
	sendErr  error
	nextID   int64
	lastEdit string
}

func (f *fakeUpstream) install(t *testing.T) *config.Settings {
	t.Helper()
	old := config.Root
	config.Root = t.TempDir()
	os.MkdirAll(config.DataPath(), 0o700)
	src := &upstream.Source{ID: "web", Title: "веб-клиент MAX", URL: "https://web.max.ru",
		Labels: []upstream.Label{{Key: "v", Text: "версия клиента"}},
		Check: func(context.Context, *upstream.Client, upstream.Values) (upstream.Values, []string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.err != nil {
				return nil, nil, f.err
			}
			return upstream.Values{"v": f.value}, []string{"Сборка сайта: 31 → 32."}, nil
		}}
	oldSrc, oldBot, oldWake := upstreamSources, upstreamBot, upstreamWake
	upstreamSources = func() []*upstream.Source { return []*upstream.Source{src} }
	upstreamWake = nil // агента нет — карточки сразу владельцу (прежний путь)
	upstreamBot = func(_ context.Context, _ *config.Settings, method string, payload any, out any) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := payload.(map[string]any)
		switch method {
		case "sendMessage":
			if f.sendErr != nil {
				return f.sendErr
			}
			f.nextID++
			f.calls = append(f.calls, "send:"+p["text"].(string))
			if out != nil {
				raw, _ := json.Marshal(map[string]int64{"message_id": f.nextID})
				json.Unmarshal(raw, out)
			}
		default:
			if method == "editMessageText" {
				f.lastEdit = p["text"].(string)
			}
			raw, _ := json.Marshal(p["message_id"])
			f.calls = append(f.calls, method+":"+string(raw))
		}
		return nil
	}
	t.Cleanup(func() { config.Root, upstreamSources, upstreamBot, upstreamWake = old, oldSrc, oldBot, oldWake })
	return &config.Settings{BotToken: "x", ApprovalChatID: 42}
}

func (f *fakeUpstream) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

func TestUpstreamFlow(t *testing.T) {
	f := &fakeUpstream{value: "26.10.6"}
	s := f.install(t)
	ctx := context.Background()

	// первая проверка — только сводка, без карточки и закрепления
	if _, err := CheckUpstream(ctx, s); err != nil {
		t.Fatal(err)
	}
	calls := f.take()
	if len(calls) != 1 || !strings.Contains(calls[0], "включено") || !strings.Contains(calls[0], "26.10.6") {
		t.Fatalf("первая проверка: %q", calls)
	}
	// без изменений — тишина
	CheckUpstream(ctx, s)
	if calls = f.take(); len(calls) != 0 {
		t.Fatalf("без изменений: %q", calls)
	}
	// изменение, а бот недоступен: не запоминаем — придёт в следующий раз
	f.value, f.sendErr = "26.11.0", errors.New("сеть")
	CheckUpstream(ctx, s)
	f.sendErr = nil
	CheckUpstream(ctx, s)
	calls = f.take()
	if len(calls) != 2 || !strings.Contains(calls[0], "<code>26.10.6</code> → <code>26.11.0</code>") ||
		!strings.Contains(calls[0], "Сборка сайта") || calls[1] != "pinChatMessage:2" {
		t.Fatalf("карточка изменения: %q", calls)
	}
	CheckUpstream(ctx, s)
	if calls = f.take(); len(calls) != 0 {
		t.Fatalf("о том же второй раз: %q", calls)
	}

	// источник сломался: владельцу — один раз, после upstreamFailNotice подряд
	f.err = errors.New("HTTP 500")
	for range upstreamFailNotice + 2 {
		CheckUpstream(ctx, s)
	}
	// …и эта карточка заменяет прежнюю того же источника: та откреплена и помечена
	calls = f.take()
	if len(calls) != 4 || !strings.Contains(calls[0], "Не удаётся проверить") || calls[1] != "pinChatMessage:3" ||
		calls[2] != "unpinChatMessage:2" || calls[3] != "editMessageText:2" {
		t.Fatalf("сбой источника: %q", calls)
	}
	if !strings.Contains(f.lastEdit, "26.11.0") || !strings.HasSuffix(f.lastEdit, "<i>Заменена новой карточкой ↓</i>") {
		t.Fatalf("пометка на заменённой: %q", f.lastEdit)
	}
	// починился, значение прежнее — тишина
	f.err = nil
	CheckUpstream(ctx, s)
	if calls = f.take(); len(calls) != 0 {
		t.Fatalf("после починки: %q", calls)
	}

	// «Разобрано» — открепить и убрать кнопку
	q := &bot.CallbackQuery{ID: "cb", Data: upstreamAck}
	q.From.ID = 42
	q.Message = &struct {
		MessageID int64 `json:"message_id"`
	}{MessageID: 3}
	oldAnswer := upstreamAnswer
	var answered string
	upstreamAnswer = func(_ context.Context, _ *config.Settings, _, text string) { answered = text }
	defer func() { upstreamAnswer = oldAnswer }()
	HandleCallback(ctx, s, q)
	if calls = f.take(); strings.Join(calls, "|") != "unpinChatMessage:3|editMessageReplyMarkup:3" || answered != "Откреплено." {
		t.Fatalf("Разобрано: %q, ответ %q", calls, answered)
	}
	// разобранную следующая карточка уже не трогает
	f.value = "26.12.0"
	CheckUpstream(ctx, s)
	if calls = f.take(); len(calls) != 2 || calls[1] != "pinChatMessage:4" {
		t.Fatalf("после «Разобрано»: %q", calls)
	}
}

// Агент разобрал изменение сам: карточка откреплена, кнопки нет, вывод дописан.
func TestUpstreamAgentAck(t *testing.T) {
	f := &fakeUpstream{value: "26.10.6"}
	s := f.install(t)
	ctx := context.Background()
	CheckUpstream(ctx, s)
	f.value = "26.11.0"
	CheckUpstream(ctx, s)
	f.take()
	if _, err := AckUpstream(ctx, s, "web", " "); err == nil {
		t.Fatal("без вывода — отказ")
	}
	cards, err := PinnedUpstream(s)
	if err != nil || len(cards) != 1 || cards[0].Source != "web" || cards[0].MessageID != 2 {
		t.Fatalf("закреплённые: %+v %v", cards, err)
	}
	done, err := AckUpstream(ctx, s, "all", "веб-клиент <не> менялся")
	if err != nil || len(done) != 1 {
		t.Fatalf("ack: %+v %v", done, err)
	}
	if calls := f.take(); strings.Join(calls, "|") != "unpinChatMessage:2|editMessageText:2" {
		t.Fatalf("вызовы: %q", calls)
	}
	if !strings.Contains(f.lastEdit, "26.11.0") || !strings.HasSuffix(f.lastEdit, "<i>✅ Разобрал агент: веб-клиент &lt;не&gt; менялся</i>") {
		t.Fatalf("пометка: %q", f.lastEdit)
	}
	if cards, _ := PinnedUpstream(s); len(cards) != 0 {
		t.Fatalf("после ack: %+v", cards)
	}
	if _, err := AckUpstream(ctx, s, "web", "ещё раз"); err == nil {
		t.Fatal("нечего откреплять — ошибка")
	}
	if done, err := AckUpstream(ctx, s, "all", "ещё раз"); err != nil || len(done) != 0 {
		t.Fatalf("all без карточек: %+v %v", done, err)
	}
}

// Изменение сначала разбирает агент: карточка без звука и не закреплена,
// агент разбужен; ему нужно решение владельца — закреплена со звуком; агента
// не разбудить — сразу владельцу.
func TestUpstreamToAgent(t *testing.T) {
	f := &fakeUpstream{value: "26.10.6"}
	s := f.install(t)
	ctx := context.Background()
	var woke []string
	var wakeErr error
	upstreamWake = func(_ context.Context, _ *config.Settings, title, text string) error {
		woke = append(woke, title+"|"+text)
		return wakeErr
	}
	CheckUpstream(ctx, s)
	f.take()
	f.value = "26.11.0"
	CheckUpstream(ctx, s)
	if calls := f.take(); len(calls) != 1 || !strings.Contains(calls[0], "Разбирает агент") {
		t.Fatalf("карточка агенту: %q", calls)
	}
	if len(woke) != 1 || !strings.HasPrefix(woke[0], "Обновление: веб-клиент MAX|") ||
		!strings.Contains(woke[0], "26.10.6 → 26.11.0") || !strings.Contains(woke[0], "updates ack web") {
		t.Fatalf("агенту: %q", woke)
	}
	if cards, _ := PinnedUpstream(s); len(cards) != 1 || !cards[0].Agent {
		t.Fatalf("открытые: %+v", cards)
	}
	// нужно решение владельца — закрепить со звуком, что решить — в карточке
	if _, err := EscalateUpstream(ctx, s, "web", "сменился вход — перелогинить?"); err != nil {
		t.Fatal(err)
	}
	if calls := f.take(); strings.Join(calls, "|") != "editMessageText:2|pinChatMessage:2" ||
		!strings.Contains(f.lastEdit, "Нужно твоё решение:</b> сменился вход") || strings.Contains(f.lastEdit, "Разбирает агент") {
		t.Fatalf("эскалация: %q %q", calls, f.lastEdit)
	}
	if cards, _ := PinnedUpstream(s); len(cards) != 1 || cards[0].Agent {
		t.Fatalf("после эскалации: %+v", cards)
	}
	AckUpstream(ctx, s, "web", "владелец решил")
	f.take()

	// агента не разбудить — карточка владельцу: пометка убрана, закреплена со звуком
	wakeErr = errors.New("нет Нестора")
	f.value = "26.12.0"
	CheckUpstream(ctx, s)
	if calls := f.take(); len(calls) != 3 || calls[1] != "editMessageText:3" || calls[2] != "pinChatMessage:3" ||
		strings.Contains(f.lastEdit, "Разбирает агент") {
		t.Fatalf("без агента: %q", calls)
	}
	// разобранную агентом карточку снимает ack без открепления
	wakeErr = nil
	f.value = "26.13.0"
	CheckUpstream(ctx, s)
	if calls := f.take(); strings.Join(calls[1:], "|") != "unpinChatMessage:3|editMessageText:3" {
		t.Fatalf("замена закреплённой: %q", calls) // прежнюю, закреплённую, снимает замена
	}
	if _, err := AckUpstream(ctx, s, "web", "не затрагивает"); err != nil {
		t.Fatal(err)
	}
	if calls := f.take(); strings.Join(calls, "|") != "editMessageText:4" { // откреплять нечего
		t.Fatalf("ack карточки агента: %q", calls)
	}
}

func TestUpstreamCardLimit(t *testing.T) {
	details := make([]string, 40)
	for i := range details {
		details[i] = "• " + strings.Repeat("коммит <b> ", 40)
	}
	card := upstreamCard(upstream.Result{Source: upstream.PyMaxCommits, Details: details,
		Diffs: []upstream.Diff{{Label: "последний коммит", Old: "a", New: "b"}}, Cur: upstream.Values{"url": "https://x/compare"}})
	if n := len([]rune(card)); n > 4096 || !strings.Contains(card, "…") || !strings.HasSuffix(card, `<a href="https://x/compare">Открыть</a>`) ||
		strings.Contains(card, "<b> ") {
		t.Fatalf("карточка %d символов:\n%s", n, card)
	}
}
