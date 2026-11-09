package tgc

import (
	"bytes"
	"compress/zlib"
	"strings"
	"testing"

	"github.com/gotd/td/tg"

	"tgagent/internal/omap"
)

func zlibbed(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w, _ := zlib.NewWriterLevel(&b, 9)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func TestWidgetInfo(t *testing.T) {
	src := `<!doctype html><html><head><title>Игра &amp; счёт</title><style>b{color:red}</style></head>` +
		`<body><h1>2048</h1><!-- скрыто --><p>Жми&nbsp;стрелки</p><script>let x="<b>не текст</b>"</script></body></html>`
	w := &tg.MessageMediaWidget{Payload: zlibbed(t, src), Height: 300, PublicData: "best=12", CanQuery: true}
	m := &tg.Message{Media: w}
	if MediaLabel(m) != "widget" {
		t.Fatalf("label %q", MediaLabel(m))
	}
	got, err := WidgetHTML(w)
	if err != nil || got != src {
		t.Fatalf("html %q %v", got, err)
	}
	s := omap.Pretty(WidgetInfo(w))
	for _, want := range []string{`"title": "Игра & счёт"`, `"text": "2048 Жми стрелки"`, `"width": "auto"`, `"height": 300`,
		`"public_data": "best=12"`, `"talks_to_bot": true`, `"script": true`} {
		if !strings.Contains(s, want) {
			t.Errorf("нет %s в %s", want, s)
		}
	}
	if strings.Contains(s, "не текст") || strings.Contains(s, "скрыто") || strings.Contains(s, "color") {
		t.Errorf("скрипт, стиль или комментарий в тексте: %s", s)
	}
}

func TestWidgetBroken(t *testing.T) {
	// gzip вместо zlib и мусор — не паника, а ошибка в поле
	w := &tg.MessageMediaWidget{Payload: []byte("not zlib")}
	if s := omap.Pretty(WidgetInfo(w)); !strings.Contains(s, `"error"`) {
		t.Fatalf("нет ошибки: %s", s)
	}
}
