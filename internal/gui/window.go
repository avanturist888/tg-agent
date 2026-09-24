package gui

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Open — поднять сервер и показать окно (или открыть страницу в браузере).
func Open(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gui", flag.ContinueOnError)
	browser := fs.Bool("browser", false, "открыть в браузере вместо окна")
	printURL := fs.Bool("print-url", false, "только напечатать адрес страницы и ждать")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// окно WebView2 живёт в потоке, который его создал
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, url, err := Start(ctx)
	if err != nil {
		return err
	}
	switch {
	case *printURL:
		fmt.Println(url)
		<-ctx.Done()
		return nil
	case *browser:
		return openBrowser(ctx, url)
	}
	if ok := openWindow(url); !ok {
		fmt.Fprintln(os.Stderr, "WebView2 недоступен — открываю в браузере.")
		return openBrowser(ctx, url)
	}
	return nil
}

func openBrowser(ctx context.Context, url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	fmt.Println("Окно управления открыто в браузере. Ctrl+C — закрыть.")
	<-ctx.Done()
	return nil
}

func dataDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "tg-agent", "webview2")
}
