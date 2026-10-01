package t3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"tgagent/internal/t3/t3test"
)

// testEnv — окружение на поддельном сервере; код выдаёт функция (как pairing_cmd).
func testEnv(t *testing.T, f *t3test.Fake) *Env {
	return &Env{Name: "fake", Origin: f.URL, TokenPath: filepath.Join(t.TempDir(), "t3-token-fake.json"),
		Model: "claude-sonnet-5-5", Project: "/workspace", PollEvery: 10 * time.Millisecond, TurnTimeout: 5 * time.Second,
		pairing: func(context.Context) (string, error) { return f.NewCode(), nil }}
}

// ── pairing_cmd: сам тестовый бинарник печатает код ─────────────────────

func TestMain(m *testing.M) {
	if code := os.Getenv("T3_TEST_PAIRING"); code != "" {
		fmt.Println("Pairing created")
		fmt.Printf(`{"id":"p1","credential":%q,"label":"tg-agent"}`+"\n", code)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestPairingCmdExchange(t *testing.T) {
	f := t3test.New(t)
	f.AddCode("cmd-code")
	t.Setenv("T3_TEST_PAIRING", "cmd-code")
	e := testEnv(t, f)
	e.pairing = nil
	exe, _ := os.Executable()
	e.PairingCmd = []string{exe}
	sh, err := e.Shell(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sh.Projects) != 2 {
		t.Fatalf("проекты: %+v", sh.Projects)
	}
	if len(f.Exchanges()) != 1 {
		t.Fatalf("обменов кода: %d", len(f.Exchanges()))
	}
	form := f.Exchanges()[0]
	if form["scope"] != tokenScopes || form["client_label"] != tokenLabel ||
		form["grant_type"] != "urn:ietf:params:oauth:grant-type:token-exchange" || form["subject_token"] != "cmd-code" {
		t.Fatalf("форма обмена: %v", form)
	}
	raw, err := os.ReadFile(e.TokenPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved savedToken
	_ = json.Unmarshal(raw, &saved)
	if saved.Token != "tok-1" || saved.Origin != f.URL || time.Until(saved.Expires) < 29*24*time.Hour {
		t.Fatalf("сохранённый токен: %+v", saved)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(e.TokenPath); st.Mode().Perm() != 0o600 {
			t.Fatalf("права файла токена: %v", st.Mode())
		}
	}
	// второй вызов — тот же токен, без нового кода
	if _, err := e.Shell(context.Background()); err != nil || f.Issued() != 1 {
		t.Fatalf("повторный вызов: err=%v, выпущено %d", err, f.Issued())
	}
}

func TestAdminPairing(t *testing.T) {
	f := t3test.New(t)
	f.SetAdmin("admin-secret")
	e := testEnv(t, f)
	e.pairing, e.AdminToken = nil, "admin-secret"
	if _, err := e.Shell(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Issued() != 1 || !strings.HasPrefix(f.Exchanges()[0]["subject_token"], "admin-code-") {
		t.Fatalf("код по admin-токену: %v", f.Exchanges())
	}
	// чужой admin-токен — понятная ошибка, а не паника
	e2 := testEnv(t, f)
	e2.pairing, e2.AdminToken = nil, "wrong"
	if _, err := e2.Shell(context.Background()); err == nil || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("неверный admin-токен: %v", err)
	}
}

func TestTokenFile(t *testing.T) {
	f := t3test.New(t)
	f.AddToken("owner-token")
	e := testEnv(t, f)
	e.pairing = nil
	e.TokenFile = filepath.Join(t.TempDir(), "tok.json")
	os.WriteFile(e.TokenFile, []byte(fmt.Sprintf(`{"origin":%q,"token":"owner-token"}`, f.URL)), 0o600)
	if _, err := e.Shell(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Issued() != 0 {
		t.Fatal("с token_file новый токен выпускать не нужно")
	}
	// токен отозвали, выпустить нечем — ошибка подсказывает, что делать
	f.RevokeAll()
	_, err := e.Shell(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pairing_cmd") {
		t.Fatalf("401 без способа выпуска: %v", err)
	}
	// строка без JSON тоже годится
	os.WriteFile(e.TokenFile, []byte("plain-token\n"), 0o600)
	f.AddToken("plain-token")
	if _, err := e.Shell(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReissueOn401(t *testing.T) {
	f := t3test.New(t)
	e := testEnv(t, f)
	ctx := context.Background()
	if _, err := e.Shell(ctx); err != nil {
		t.Fatal(err)
	}
	f.RevokeAll() // владелец отозвал сессию tg-agent в T3
	id, err := e.CreateThread(ctx, "TG: Избранное")
	if err != nil {
		t.Fatalf("после 401 ждали перевыпуск токена: %v", err)
	}
	if f.Issued() != 2 || id == "" {
		t.Fatalf("выпущено токенов %d, тред %q", f.Issued(), id)
	}
	raw, _ := os.ReadFile(e.TokenPath)
	if !strings.Contains(string(raw), "tok-2") {
		t.Fatal("новый токен не сохранён")
	}
}

func TestCreateThreadTurnAndWait(t *testing.T) {
	f := t3test.New(t)
	f.LagPolls = 2 // T3 сначала показывает прошлый ход
	e := testEnv(t, f)
	ctx := context.Background()
	id, err := e.CreateThread(ctx, "TG: Избранное")
	if err != nil {
		t.Fatal(err)
	}
	cmd := f.Last("thread.create")
	ms, _ := cmd["modelSelection"].(map[string]any)
	if cmd["projectId"] != "p-main" || cmd["runtimeMode"] != "full-access" || cmd["title"] != "TG: Избранное" ||
		ms["instanceId"] != "claudeAgent" || ms["model"] != "claude-sonnet-5-5" || cmd["branch"] != nil {
		t.Fatalf("thread.create: %v", cmd)
	}

	// первый ход
	prev, err := e.StartTurn(ctx, id, "Ответь одним словом: готов")
	if err != nil || prev != "" {
		t.Fatalf("первый ход: prev=%q err=%v", prev, err)
	}
	res, err := e.WaitTurn(ctx, id, prev)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "completed" || res.Text != "готов" || res.TurnID == "" {
		t.Fatalf("итог хода: %+v", res)
	}
	turn := f.Last("thread.turn.start")
	if turn["runtimeMode"] != "full-access" || turn["message"].(map[string]any)["text"] != "Ответь одним словом: готов" {
		t.Fatalf("thread.turn.start: %v", turn)
	}

	// второй ход: прошлый (завершённый) не должен сойти за новый
	prev2, err := e.StartTurn(ctx, id, "ещё раз")
	if err != nil || prev2 != res.TurnID {
		t.Fatalf("второй ход: prev=%q err=%v", prev2, err)
	}
	res2, err := e.WaitTurn(ctx, id, prev2)
	if err != nil || res2.TurnID == res.TurnID || res2.State != "completed" {
		t.Fatalf("второй ход: %+v %v", res2, err)
	}
}

func TestStartTurnBusyAndGone(t *testing.T) {
	f := t3test.New(t)
	f.FinishAfter = 0 // ход не кончается
	e := testEnv(t, f)
	ctx := context.Background()
	id, err := e.CreateThread(ctx, "TG: x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.StartTurn(ctx, id, "раз"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.StartTurn(ctx, id, "два"); !errors.Is(err, Busy) {
		t.Fatalf("ход идёт: ждали Busy, получили %v", err)
	}
	// ожидание упирается в таймаут
	e.TurnTimeout = 100 * time.Millisecond
	if _, err := e.WaitTurn(ctx, id, ""); !errors.Is(err, TurnTimeout) {
		t.Fatalf("бесконечный ход: ждали TurnTimeout, получили %v", err)
	}

	// удалённый тред — 404
	if _, err := e.Thread(ctx, "нет-такого"); !errors.Is(err, ThreadGone) {
		t.Fatalf("404: ждали ThreadGone, получили %v", err)
	}
	if _, err := e.StartTurn(ctx, "нет-такого", "x"); !errors.Is(err, ThreadGone) {
		t.Fatalf("ход в удалённом треде: %v", err)
	}
	// тред в архиве — тоже нет
	f.Archive(id)
	if _, err := e.StartTurn(ctx, id, "x"); !errors.Is(err, ThreadGone) {
		t.Fatalf("архив: ждали ThreadGone, получили %v", err)
	}
}

func TestWaitTurnError(t *testing.T) {
	f := t3test.New(t)
	f.FinalState = "error"
	e := testEnv(t, f)
	ctx := context.Background()
	id, _ := e.CreateThread(ctx, "TG: x")
	prev, err := e.StartTurn(ctx, id, "x")
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.WaitTurn(ctx, id, prev)
	if err != nil || res.State != "error" || res.Error != "провайдер упал" {
		t.Fatalf("ход с ошибкой: %+v %v", res, err)
	}
}

func TestProjectAndModel(t *testing.T) {
	f := t3test.New(t)
	e := testEnv(t, f)
	e.Project = "/nope"
	if _, err := e.CreateThread(context.Background(), "TG: x"); err == nil || !strings.Contains(err.Error(), "/nope") {
		t.Fatalf("нет проекта: %v", err)
	}
	e.Project, e.Model = "", ""
	if _, err := e.CreateThread(context.Background(), "TG: x"); err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("нет модели: %v", err)
	}
}

func TestCredentialParsing(t *testing.T) {
	if got := credential([]byte("Pairing created\n{\"credential\":\"abc\"}\n")); got != "abc" {
		t.Fatalf("credential: %q", got)
	}
	if credential([]byte("мусор")) != "" {
		t.Fatal("из мусора кода нет")
	}
}
