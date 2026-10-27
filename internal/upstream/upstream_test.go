package upstream

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fake — подставные web.max.ru, каталог сборок и GitHub.
type fake struct {
	mu       sync.Mutex
	build    string
	ver      string // "" — в скриптах нет констант
	verFirst bool   // версия перед адресом WebSocket (как в сборке 32)
	catalog  string
	head     string
	compare  string // "" — 404 (ветку переписали)
	pageHits int
}

func (f *fake) server(t *testing.T) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /web/_app/version.json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		fmt.Fprintf(w, `{"version":%q}`, f.build)
	})
	mux.HandleFunc("GET /web/{$}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.pageHits++
		f.mu.Unlock()
		fmt.Fprint(w, `<link href="/_app/immutable/entry/start.A1.js"><link href="/_app/immutable/chunks/B2.js">`+
			`<link href="/_app/immutable/entry/start.A1.js">`)
	})
	mux.HandleFunc("GET /web/_app/immutable/entry/start.A1.js", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "import`x`;var a=`1.2.3`")
	})
	mux.HandleFunc("GET /web/_app/immutable/chunks/B2.js", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.ver == "" {
			fmt.Fprint(w, "var Qr=`wss://api.oneme.ru/websocket`;")
			return
		}
		if f.verFirst {
			fmt.Fprintf(w, "var a=`9.9.9`;Z(`<svg>`);var $r=`D9QQ`,ei=`%s`,ti=`wss://api.oneme.ru/websocket`,ni=`20516`,ri=`CNHI`,si=`1.0.0`;", f.ver)
			return
		}
		fmt.Fprintf(w, "js\";var Qr=`wss://api.oneme.ru/websocket`,$r=`D9QQ`,ei=`20420`,ti=`CNHI`,ni=`%s`;Z(`<svg>`)", f.ver)
	})
	mux.HandleFunc("GET /hashes", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		fmt.Fprint(w, f.catalog)
	})
	mux.HandleFunc("GET /gh/repos/MaxApiTeam/PyMax/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v2.4.1","name":"PyMax 2.4.1","html_url":"https://x/rel","body":"- one\r\n- two"}`)
	})
	mux.HandleFunc("GET /gh/repos/MaxApiTeam/PyMax/commits", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		fmt.Fprintf(w, `[{"sha":%q,"html_url":"https://x/c","commit":{"message":"fix: twine bump\n\nbody"}}]`, f.head)
	})
	mux.HandleFunc("GET /gh/repos/MaxApiTeam/PyMax/compare/{spec}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.compare == "" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, f.compare)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), MaxWeb: srv.URL + "/web", Hashes: srv.URL + "/hashes", GitHub: srv.URL + "/gh"}
}

func one(t *testing.T, c *Client, st *State, src *Source) Result {
	t.Helper()
	r := Check(context.Background(), c, st, []*Source{src})[0]
	if r.Err != nil {
		t.Fatalf("%s: %v", src.ID, r.Err)
	}
	st.Apply(r, time.Now())
	return r
}

func TestMaxWeb(t *testing.T) {
	f := &fake{build: "31", ver: "26.10.6"}
	c, st := f.server(t), &State{Sources: map[string]*SourceState{}}

	r := one(t, c, st, MaxWeb)
	if !r.First || r.Changed() || r.Cur["app_version"] != "26.10.6" || r.Cur["ws_url"] != "wss://api.oneme.ru/websocket" {
		t.Fatalf("первая проверка: %+v", r)
	}
	// та же выкладка: страницу и скрипты не качаем
	if r = one(t, c, st, MaxWeb); r.Changed() || f.pageHits != 1 {
		t.Fatalf("повтор: changed=%v, страница скачана %d раз", r.Changed(), f.pageHits)
	}
	// новая выкладка с той же версией — не новость
	f.build = "32"
	if r = one(t, c, st, MaxWeb); r.Changed() || f.pageHits != 2 {
		t.Fatalf("новая сборка без новой версии: %+v", r)
	}
	f.build, f.ver = "33", "26.11.0"
	r = one(t, c, st, MaxWeb)
	if !r.Changed() || len(r.Diffs) != 1 || r.Diffs[0].Old != "26.10.6" || r.Diffs[0].New != "26.11.0" ||
		!strings.Contains(strings.Join(r.Details, " "), "32 → 33") {
		t.Fatalf("новая версия: %+v", r)
	}
	// сайт перестроили — версии не нашли: это тоже повод посмотреть
	f.build, f.ver = "34", ""
	r = one(t, c, st, MaxWeb)
	if !r.Changed() || r.Cur["app_version"] != notFound || !strings.Contains(strings.Join(r.Details, " "), "вручную") {
		t.Fatalf("версия не найдена: %+v", r)
	}
	// та же выкладка, но версию не нашли — скрипты качаем снова
	f.ver, f.verFirst = "26.10.7", true
	r = one(t, c, st, MaxWeb)
	if !r.Changed() || r.Diffs[0].Old != notFound || r.Diffs[0].New != "26.10.7" || f.pageHits != 5 || len(r.Details) != 0 {
		t.Fatalf("повторный поиск в той же сборке: %+v, страница скачана %d раз", r, f.pageHits)
	}
	if r = one(t, c, st, MaxWeb); r.Changed() || f.pageHits != 5 {
		t.Fatalf("нашли — снова не качаем: %+v", r)
	}
}

func TestAndroidBuilds(t *testing.T) {
	f := &fake{catalog: `{"26.10.0":{"build_number":6648},"26.37.0":{"build_number":6886}}`}
	c, st := f.server(t), &State{Sources: map[string]*SourceState{}}
	if r := one(t, c, st, AndroidBuilds); r.Cur["latest"] != "26.37.0 (сборка 6886)" {
		t.Fatalf("последняя: %+v", r.Cur)
	}
	// старую запись дописали — молча
	f.catalog = `{"26.9.1":{"build_number":6643},"26.10.0":{"build_number":6648},"26.37.0":{"build_number":6886}}`
	if r := one(t, c, st, AndroidBuilds); r.Changed() {
		t.Fatalf("добавили старую сборку — не новость: %+v", r.Diffs)
	}
	f.catalog = `{"26.9.1":{"build_number":6643},"26.10.0":{"build_number":6648},"26.37.0":{"build_number":6886},` +
		`"26.38.0":{"build_number":6890},"26.38.1":{"build_number":6893}}`
	r := one(t, c, st, AndroidBuilds)
	if !r.Changed() || r.Diffs[0].New != "26.38.1 (сборка 6893)" ||
		r.Details[0] != "Новые сборки в каталоге: 26.38.0 (6890), 26.38.1 (6893)." {
		t.Fatalf("новая сборка: %+v", r)
	}
}

func TestPyMaxCommits(t *testing.T) {
	f := &fake{head: "1111111aaaa"}
	c, st := f.server(t), &State{Sources: map[string]*SourceState{}}
	if r := one(t, c, st, PyMaxCommits); r.Cur["head"] != "1111111 · fix: twine bump" {
		t.Fatalf("голова: %+v", r.Cur)
	}
	f.head = "2222222bbbb"
	f.compare = `{"html_url":"https://x/compare","total_commits":2,"commits":[{"commit":{"message":"feat: old"}},` +
		`{"commit":{"message":"fix: new\n\nbody"}}],"files":[{"filename":"src/pymax/protocol/enums.py"},` +
		`{"filename":"README.md"},{"filename":"src/pymax/api/messages/payloads.py"}]}`
	r := one(t, c, st, PyMaxCommits)
	want := []string{"Новых коммитов: 2.", "• fix: new", "• feat: old",
		"⚠️ Затронуты протокол или вход: protocol/enums.py, api/messages/payloads.py."}
	if !r.Changed() || r.Cur["url"] != "https://x/compare" || strings.Join(r.Details, "|") != strings.Join(want, "|") {
		t.Fatalf("новые коммиты: %q %+v", r.Details, r.Cur)
	}
	f.head, f.compare = "3333333cccc", ""
	if r = one(t, c, st, PyMaxCommits); !r.Changed() || !strings.Contains(r.Details[0], "force-push") {
		t.Fatalf("переписанная ветка: %+v", r)
	}
}

func TestPyMaxRelease(t *testing.T) {
	c, st := (&fake{}).server(t), &State{Sources: map[string]*SourceState{}}
	r := one(t, c, st, PyMaxRelease)
	if r.Cur["tag"] != "v2.4.1" || r.Cur["url"] != "https://x/rel" || strings.Join(r.Details, "|") != "PyMax 2.4.1|- one\n- two" {
		t.Fatalf("релиз: %+v %q", r.Cur, r.Details)
	}
}

func TestApplyAndFailures(t *testing.T) {
	c, st := (&fake{}).server(t), &State{Sources: map[string]*SourceState{}}
	bad := &Source{ID: "bad", Check: func(ctx context.Context, c *Client, prev Values) (Values, []string, error) {
		_, err := c.get(ctx, c.MaxWeb+"/nope", nil)
		return nil, nil, err
	}}
	for range 2 {
		r := Check(context.Background(), c, st, []*Source{bad})[0]
		if r.Err == nil || r.Changed() {
			t.Fatalf("ждали ошибку: %+v", r)
		}
		st.Apply(r, time.Now())
	}
	if ss := st.Sources["bad"]; ss.Fails != 2 || !strings.Contains(ss.Error, "HTTP 404") || ss.Values != nil {
		t.Fatalf("состояние после ошибок: %+v", ss)
	}
	path := t.TempDir() + "/upstream.json"
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil || back.Sources["bad"].Fails != 2 {
		t.Fatalf("сохранение: %+v %v", back, err)
	}
	if empty, err := Load(t.TempDir() + "/none.json"); err != nil || len(empty.Sources) != 0 {
		t.Fatalf("нет файла: %+v %v", empty, err)
	}
}
