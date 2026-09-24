//go:build windows

package gui

import webview2 "github.com/jchv/go-webview2"

// openWindow — нативное окно WebView2; false — рантайма нет.
func openWindow(url string) bool {
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		DataPath:  dataDir(),
		WindowOptions: webview2.WindowOptions{
			Title:  "tg-agent — управление",
			Width:  1200,
			Height: 840,
			Center: true,
		},
	})
	if w == nil {
		return false
	}
	defer w.Destroy()
	w.Navigate(url)
	w.Run()
	return true
}
