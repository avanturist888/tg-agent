package upstream

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Sources — что проверяем, в порядке карточек.
func Sources() []*Source { return []*Source{MaxWeb, AndroidBuilds, PyMaxRelease, PyMaxCommits} }

const pymaxRepo = "MaxApiTeam/PyMax"

// ── веб-клиент MAX ───────────────────────────────────────────────────────

// MaxWeb — web.max.ru (SvelteKit): номер сборки в /_app/version.json меняется
// с каждой выкладкой; тогда в её скриптах ищем адрес WebSocket и версию,
// которую клиент называет при входе (`appVersion`, рядом с адресом).
var MaxWeb = &Source{
	ID: "max-web", Title: "веб-клиент MAX", URL: "https://web.max.ru",
	Labels: []Label{{"app_version", "версия клиента"}, {"ws_url", "адрес WebSocket"}},
	Check:  checkMaxWeb,
}

var (
	webChunk = regexp.MustCompile(`/_app/immutable/[A-Za-z0-9_\-./]+\.js`)
	webWS    = regexp.MustCompile("`(wss://[^`\\s]+/websocket)`")
	webVer   = regexp.MustCompile("`(\\d+\\.\\d+\\.\\d+)`")
)

const (
	notFound   = "не найдена"
	notFoundWS = "не найден"
	// версия — константа рядом с адресом WebSocket, в одном var; порядок
	// сборщик меняет от выкладки к выкладке:
	// 31: `wss://api.oneme.ru/websocket`,$r=`…`,ei=`20420`,ti=`…`,ni=`26.10.6`
	// 32: $r=`…`,ei=`26.10.7`,ti=`wss://api.oneme.ru/websocket`,ni=`20516`
	webVerWindow = 400
)

// webConsts — адрес WebSocket и ближайшая к нему версия из скрипта сайта.
func webConsts(js []byte) (ws, ver string, ok bool) {
	for _, m := range webWS.FindAllSubmatchIndex(js, -1) {
		lo, hi := max(0, m[0]-webVerWindow), min(len(js), m[1]+webVerWindow)
		dist := -1
		for _, v := range webVer.FindAllSubmatchIndex(js[lo:hi], -1) {
			start, end := lo+v[0], lo+v[1]
			var d int
			switch {
			case end <= m[0]:
				d = m[0] - end
			case start >= m[1]:
				d = start - m[1]
			default:
				continue
			}
			if dist < 0 || d < dist {
				dist, ver = d, string(js[lo+v[2]:lo+v[3]])
			}
		}
		if dist >= 0 {
			return string(js[m[2]:m[3]]), ver, true
		}
	}
	return "", "", false
}

func checkMaxWeb(ctx context.Context, c *Client, prev Values) (Values, []string, error) {
	raw, err := c.get(ctx, c.MaxWeb+"/_app/version.json", nil)
	if err != nil {
		return nil, nil, err
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.Version == "" {
		return nil, nil, fmt.Errorf("version.json: нет номера сборки")
	}
	// та же выкладка — скрипты не качаем; не нашли в ней версию — ищем снова
	if prev != nil && prev["build"] == v.Version && prev["app_version"] != "" && prev["app_version"] != notFound {
		return prev, nil, nil
	}
	page, err := c.get(ctx, c.MaxWeb+"/", nil)
	if err != nil {
		return nil, nil, err
	}
	cur := Values{"build": v.Version, "app_version": notFound, "ws_url": notFoundWS}
	seen := map[string]bool{}
	for _, path := range webChunk.FindAllString(string(page), -1) {
		if seen[path] {
			continue
		}
		seen[path] = true
		js, err := c.get(ctx, c.MaxWeb+path, nil)
		if err != nil {
			return nil, nil, err
		}
		if ws, ver, ok := webConsts(js); ok {
			cur["ws_url"], cur["app_version"] = ws, ver
			break
		}
	}
	var details []string
	if prev != nil && prev["build"] != v.Version {
		details = append(details, fmt.Sprintf("Сборка сайта: %s → %s.", prev["build"], v.Version))
	}
	if cur["app_version"] == notFound {
		details = append(details, "Версию в скриптах сайта найти не удалось — похоже, сборка устроена иначе; проверь вручную.")
	}
	return cur, details, nil
}

// ── сборки Android-клиента ───────────────────────────────────────────────

// AndroidBuilds — каталог хэшей APK от авторов PyMax: новая запись в нём —
// вышла новая версия Android-клиента MAX (и мобильный вход PyMax её знает).
var AndroidBuilds = &Source{
	ID: "android-builds", Title: "Android-клиент MAX", URL: "https://hashes.pymax.org/versions.json",
	Labels: []Label{{"latest", "последняя версия"}},
	Check:  checkAndroid,
}

func checkAndroid(ctx context.Context, c *Client, prev Values) (Values, []string, error) {
	raw, err := c.get(ctx, c.Hashes, nil)
	if err != nil {
		return nil, nil, err
	}
	var cat map[string]struct {
		Build int `json:"build_number"`
	}
	if err := json.Unmarshal(raw, &cat); err != nil {
		return nil, nil, fmt.Errorf("каталог сборок: %w", err)
	}
	if len(cat) == 0 {
		return nil, nil, fmt.Errorf("каталог сборок пуст")
	}
	type build struct {
		ver string
		n   int
	}
	all := make([]build, 0, len(cat))
	for ver, b := range cat {
		all = append(all, build{ver, b.Build})
	}
	slices.SortFunc(all, func(a, b build) int { return cmp.Or(cmp.Compare(a.n, b.n), strings.Compare(a.ver, b.ver)) })
	names := make([]string, len(all))
	for i, b := range all {
		names[i] = b.ver
	}
	last := all[len(all)-1]
	cur := Values{"latest": fmt.Sprintf("%s (сборка %d)", last.ver, last.n), "versions": strings.Join(names, ",")}
	var details []string
	if prev != nil {
		old := map[string]bool{}
		for _, v := range strings.Split(prev["versions"], ",") {
			old[v] = true
		}
		var added []string
		for _, b := range all {
			if !old[b.ver] {
				added = append(added, fmt.Sprintf("%s (%d)", b.ver, b.n))
			}
		}
		if len(added) > 0 {
			details = append(details, "Новые сборки в каталоге: "+strings.Join(added, ", ")+".")
		}
	}
	return cur, details, nil
}

// ── PyMax на GitHub ──────────────────────────────────────────────────────

var PyMaxRelease = &Source{
	ID: "pymax-release", Title: "релиз PyMax", URL: "https://github.com/" + pymaxRepo + "/releases",
	Labels: []Label{{"tag", "релиз"}},
	Check:  checkRelease,
}

var PyMaxCommits = &Source{
	ID: "pymax-main", Title: "PyMax: основная ветка", URL: "https://github.com/" + pymaxRepo + "/commits",
	Labels: []Label{{"head", "последний коммит"}},
	Check:  checkCommits,
}

var ghHeaders = map[string]string{"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"}

func checkRelease(ctx context.Context, c *Client, prev Values) (Values, []string, error) {
	raw, err := c.get(ctx, c.GitHub+"/repos/"+pymaxRepo+"/releases/latest", ghHeaders)
	if err != nil {
		return nil, nil, err
	}
	var rel struct {
		Tag  string `json:"tag_name"`
		Name string `json:"name"`
		URL  string `json:"html_url"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(raw, &rel); err != nil || rel.Tag == "" {
		return nil, nil, fmt.Errorf("релиз PyMax: нет тега")
	}
	cur := Values{"tag": rel.Tag, "url": rel.URL}
	var details []string
	if rel.Name != "" && rel.Name != rel.Tag {
		details = append(details, rel.Name)
	}
	if notes := firstLines(rel.Body, 8, 600); notes != "" {
		details = append(details, notes)
	}
	return cur, details, nil
}

// sensitive — файлы PyMax, правка которых затрагивает протокол или вход:
// их стоит сверить с нашим клиентом.
var sensitive = []string{
	"src/pymax/protocol/", "src/pymax/transport/", "src/pymax/connection/",
	"src/pymax/auth/", "src/pymax/api/session/", "src/pymax/api/auth/",
	"src/pymax/config.py", "src/pymax/_data/", "/payloads.py",
}

func isSensitive(name string) bool {
	for _, p := range sensitive {
		if strings.HasPrefix(name, p) || (strings.HasPrefix(p, "/") && strings.HasSuffix(name, p)) {
			return true
		}
	}
	return false
}

const maxSubjects = 10

func checkCommits(ctx context.Context, c *Client, prev Values) (Values, []string, error) {
	raw, err := c.get(ctx, c.GitHub+"/repos/"+pymaxRepo+"/commits?per_page=1", ghHeaders)
	if err != nil {
		return nil, nil, err
	}
	var head []struct {
		SHA    string `json:"sha"`
		URL    string `json:"html_url"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(raw, &head); err != nil || len(head) == 0 || len(head[0].SHA) < 7 {
		return nil, nil, fmt.Errorf("коммиты PyMax: пустой ответ")
	}
	h := head[0]
	cur := Values{"head": h.SHA[:7] + " · " + subject(h.Commit.Message), "sha": h.SHA, "url": h.URL}
	if prev == nil || prev["sha"] == "" || prev["sha"] == h.SHA {
		return cur, nil, nil
	}
	raw, err = c.get(ctx, c.GitHub+"/repos/"+pymaxRepo+"/compare/"+prev["sha"]+"..."+h.SHA, ghHeaders)
	var he *HTTPError
	if errors.As(err, &he) && he.Status == 404 {
		return cur, []string{"Прежнего коммита в истории нет — ветку переписали (force-push)."}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var cmpr struct {
		URL     string `json:"html_url"`
		Total   int    `json:"total_commits"`
		Commits []struct {
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		} `json:"commits"`
		Files []struct {
			Name string `json:"filename"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &cmpr); err != nil {
		return nil, nil, fmt.Errorf("сравнение коммитов: %w", err)
	}
	if cmpr.URL != "" {
		cur["url"] = cmpr.URL
	}
	details := []string{fmt.Sprintf("Новых коммитов: %d.", max(cmpr.Total, len(cmpr.Commits)))}
	// GitHub отдаёт от старых к новым; показываем свежие сверху
	for i, n := len(cmpr.Commits)-1, 0; i >= 0 && n < maxSubjects; i, n = i-1, n+1 {
		details = append(details, "• "+subject(cmpr.Commits[i].Commit.Message))
	}
	if rest := len(cmpr.Commits) - maxSubjects; rest > 0 {
		details = append(details, fmt.Sprintf("…и ещё %d.", rest))
	}
	var touched []string
	for _, f := range cmpr.Files {
		if isSensitive(f.Name) {
			touched = append(touched, strings.TrimPrefix(f.Name, "src/pymax/"))
		}
	}
	if len(touched) > 0 {
		details = append(details, "⚠️ Затронуты протокол или вход: "+strings.Join(touched, ", ")+".")
	}
	return cur, details, nil
}

func subject(msg string) string {
	s, _, _ := strings.Cut(strings.TrimSpace(msg), "\n")
	return strings.TrimSpace(s)
}

// firstLines — начало заметок к релизу: не больше n строк и limit символов.
func firstLines(text string, n, limit int) string {
	var out []string
	size := 0
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if len(out) == n || size+len([]rune(l)) > limit {
			out = append(out, "…")
			break
		}
		out = append(out, l)
		size += len([]rune(l))
	}
	return strings.Join(out, "\n")
}
