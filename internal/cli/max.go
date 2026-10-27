package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"
	"golang.org/x/term"

	"tgagent/internal/config"
	"tgagent/internal/maxc"
	"tgagent/internal/upstream"
)

const maxUsage = `tg max — аккаунт в мессенджере MAX (вход как веб-клиент, web.max.ru).

  tg max login          вход по QR: отсканировать в приложении MAX на телефоне;
                        облачный пароль спросит здесь же, скрытым вводом
  tg max import [файл]  перенести сессию PyMax (по умолчанию data\max\pymax-web.db)
  tg max status         жив ли вход: профиль и число чатов (--chats — список)

Сессия — data\max\session.json: это полный доступ к аккаунту, никому не давать.`

func cmdMax(ctx context.Context, args []string) int {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "login":
		return maxLogin(ctx, args)
	case "import":
		return maxImport(ctx, args)
	case "status", "":
		return maxStatus(ctx, args)
	case "-h", "--help", "help":
		fmt.Println(maxUsage)
		return 0
	}
	fmt.Fprintln(os.Stderr, maxUsage)
	return 2
}

func maxDir() string         { return filepath.Join(config.DataPath(), "max") }
func maxSessionPath() string { return filepath.Join(maxDir(), "session.json") }

var semver = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// maxWebVersion — версия веб-клиента, которую видело слежение за
// обновлениями (data/upstream.json); не видело — запасная.
func maxWebVersion() string {
	st, err := upstream.Load(filepath.Join(config.DataPath(), "upstream.json"))
	if err == nil {
		if ss := st.Sources[upstream.MaxWeb.ID]; ss != nil && semver.MatchString(ss.Values["app_version"]) {
			return ss.Values["app_version"]
		}
	}
	return maxc.FallbackWebVersion
}

func maxLogin(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("max login", flag.ContinueOnError)
	force := fs.Bool("force", false, "войти заново, даже если сессия есть")
	if _, err := parse(fs, args); err != nil {
		return 2
	}
	if old, err := maxc.LoadSession(maxSessionPath()); err == nil && !*force {
		fmt.Printf("Вход в MAX уже есть (с %s). Проверить — tg max status; войти заново — tg max login --force.\n",
			old.Created.Local().Format("02.01.2006 15:04"))
		return 0
	}
	sess := &maxc.Session{DeviceID: maxc.NewDeviceID(), UserAgent: maxc.WebUserAgent(maxWebVersion()), Source: "qr"}
	qrPath := filepath.Join(maxDir(), "qr.png")
	defer os.Remove(qrPath)
	in := bufio.NewReader(os.Stdin)

	for attempt := 1; ; attempt++ {
		c, err := maxc.Dial(ctx, maxc.Options{UserAgent: sess.UserAgent.HeaderUserAgent})
		if err != nil {
			return fail(err)
		}
		if err := c.Handshake(ctx, sess.UserAgent, sess.DeviceID); err != nil {
			c.Close()
			return fail(err)
		}
		token, err := c.LoginQR(ctx, func(q maxc.QR) error { return showQR(q, qrPath) }, func(hint string, prev error) (string, error) {
			if prev != nil {
				fmt.Println("Пароль не принят:", prev)
			}
			return askMaxPassword(in, hint)
		})
		if errors.Is(err, maxc.ErrQRExpired) && attempt < 10 {
			c.Close()
			fmt.Println("\nQR-код истёк — вот новый.")
			continue
		}
		if err != nil {
			c.Close()
			return fail(err)
		}
		r, err := c.Login(ctx, token)
		c.Close()
		if err != nil {
			return fail(err)
		}
		sess.Token, sess.UserID, sess.Created = token, r.Profile.Contact.ID, time.Now()
		if err := sess.Save(maxSessionPath()); err != nil {
			return fail(err)
		}
		fmt.Printf("\nВход в MAX выполнен: %s, чатов %d. Сессия — %s\n",
			r.Profile.Contact.DisplayName(), len(r.Chats), maxSessionPath())
		return 0
	}
}

// showQR — QR в консоли и картинкой рядом с сессией.
func showQR(q maxc.QR, png string) error {
	code, err := qrcode.New(q.Link, qrcode.Medium)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(png), 0o700); err != nil {
		return err
	}
	if err := code.WriteFile(320, png); err != nil {
		return err
	}
	fmt.Println(code.ToSmallString(false))
	fmt.Printf("Отсканируй QR-код в приложении MAX на телефоне.\n"+
		"Плохо видно — открой картинку: %s\nКод действует до %s.\n", png, q.ExpiresAt().Format("15:04:05"))
	return nil
}

func askMaxPassword(in *bufio.Reader, hint string) (string, error) {
	if hint != "" {
		fmt.Printf("Подсказка к паролю: %s\n", hint)
	}
	fmt.Print("Облачный пароль MAX: ")
	if term.IsTerminal(int(os.Stdin.Fd())) {
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		return string(raw), err
	}
	line, err := in.ReadString('\n')
	return strings.TrimSpace(line), err
}

func maxImport(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("max import", flag.ContinueOnError)
	force := fs.Bool("force", false, "заменить имеющуюся сессию")
	rest, err := parse(fs, args)
	if err != nil {
		return 2
	}
	src := filepath.Join(maxDir(), "pymax-web.db")
	if len(rest) > 0 {
		src = rest[0]
	}
	if _, err := maxc.LoadSession(maxSessionPath()); err == nil && !*force {
		fmt.Println("Сессия MAX уже есть; заменить — tg max import --force.")
		return 1
	}
	sess, err := maxc.ImportPyMax(ctx, src)
	if err != nil {
		return fail(err)
	}
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	c, r, err := maxc.Open(cctx, sess, maxc.Options{})
	if err != nil {
		return fail(fmt.Errorf("сессия из %s не подошла: %w", src, err))
	}
	c.Close()
	sess.UserID = r.Profile.Contact.ID
	if err := sess.Save(maxSessionPath()); err != nil {
		return fail(err)
	}
	fmt.Printf("Сессия PyMax перенесена: %s, чатов %d. Теперь она — %s\n",
		r.Profile.Contact.DisplayName(), len(r.Chats), maxSessionPath())
	return 0
}

func maxStatus(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("max status", flag.ContinueOnError)
	list := fs.Bool("chats", false, "показать чаты")
	if _, err := parse(fs, args); err != nil {
		return 2
	}
	sess, err := maxc.LoadSession(maxSessionPath())
	if err != nil {
		return fail(err)
	}
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	c, r, err := maxc.Open(cctx, sess, maxc.Options{})
	if errors.Is(err, maxc.ErrTokenRevoked) {
		fmt.Println("Сессия MAX завершена на стороне MAX (выход на телефоне или «со всех устройств»). Войти заново: tg max login --force")
		return 1
	}
	if err != nil {
		return fail(err)
	}
	c.Close()
	types := map[string]int{}
	for _, ch := range r.Chats {
		types[ch.Type]++
	}
	fmt.Printf("MAX: %s (id %d), вход с %s (%s), веб-клиент %s.\nЧатов: %d %v\n",
		r.Profile.Contact.DisplayName(), r.Profile.Contact.ID, sess.Created.Local().Format("02.01.2006 15:04"),
		sess.Source, sess.UserAgent.AppVersion, len(r.Chats), types)
	if *list {
		chats := r.Chats
		sort.SliceStable(chats, func(i, j int) bool { return chats[i].LastEventTime > chats[j].LastEventTime })
		for _, ch := range chats {
			fmt.Printf("  %-20d %-8s %s\n", ch.ID, ch.Type, ch.Title)
		}
	}
	return 0
}
