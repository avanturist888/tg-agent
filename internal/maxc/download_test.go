package maxc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Видео MAX лежит на okcdn, ссылка подписана под браузер: с User-Agent
// Go-http-client CDN отвечал 400 «10» (10.11.2026, кружки не расшифровывались).
func TestDownloadSendsBrowserAgent(t *testing.T) {
	ua := WebUserAgent("").HeaderUserAgent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != ua || r.Header.Get("Referer") != DefaultOrigin+"/" {
			w.WriteHeader(400)
			w.Write([]byte("10"))
			return
		}
		w.Write([]byte("video"))
	}))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "v.mp4")
	if _, err := Download(context.Background(), srv.URL, dst, 1<<20, ua); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "video" {
		t.Fatalf("%q", b)
	}
	_, err := Download(context.Background(), srv.URL, dst+"2", 1<<20, "")
	if err == nil || !strings.Contains(err.Error(), "400: 10") || strings.Contains(err.Error(), "?") {
		t.Fatalf("ошибка без ответа сервера или с адресом: %v", err)
	}
}
