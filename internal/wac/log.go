package wac

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// warnLog — предупреждения и ошибки whatsmeow в вывод службы
// (data/service.log). Узлы протокола (XML) в сообщение не попадают: в них
// бывают токены и ключи (например, privacy_token в предупреждении о
// медленной обработке узла), whatsmeow маскирует не все.
type warnLog struct{ module string }

func newWarnLog(module string) waLog.Logger { return warnLog{module} }

func (l warnLog) Warnf(msg string, args ...any)  { l.write("WARN", msg, args) }
func (l warnLog) Errorf(msg string, args ...any) { l.write("ERROR", msg, args) }
func (warnLog) Infof(string, ...any)             {}
func (warnLog) Debugf(string, ...any)            {}
func (l warnLog) Sub(module string) waLog.Logger { return warnLog{l.module + "/" + module} }

func (l warnLog) write(level, msg string, args []any) {
	fmt.Fprintf(os.Stdout, "%s [%s %s] %s\n", time.Now().Format("15:04:05.000"), l.module, level, scrubNodes(fmt.Sprintf(msg, args...)))
}

// адрес с путём и параметрами: у ссылок на медиа в них подпись доступа
var (
	urlRe = regexp.MustCompile(`(?i)\b((?:https?|wss?)://[^/\s"?#]+)[^\s"]*`)
	// параметры запроса где угодно (и в путях без схемы — directPath):
	// подпись доступа к медиа WhatsApp (oh=, oe=) лежит в них
	queryRe = regexp.MustCompile(`\?[^\s"]*`)
)

// scrubNodes — всё, начиная с первого узла XML, заменить пометкой, а от
// адресов оставить только хост (ошибки сети печатают URL целиком).
func scrubNodes(s string) string {
	if i := strings.IndexByte(s, '<'); i >= 0 {
		s = strings.TrimRight(s[:i], " :") + " [узел скрыт]"
	}
	s = urlRe.ReplaceAllString(s, "$1/…")
	return queryRe.ReplaceAllString(s, "?…")
}
