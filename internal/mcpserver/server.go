// Package mcpserver — MCP-сервер (stdio): то, что видит агент.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"tgagent/internal/attach"
	"tgagent/internal/config"
	"tgagent/internal/core"
	"tgagent/internal/lock"
	"tgagent/internal/omap"
	"tgagent/internal/outbox"
	"tgagent/internal/svc"
	"tgagent/internal/tgc"
)

const instructions = "Доступ к Telegram владельца, ограниченный белым списком чатов. " +
	"Начинай с tg_list_chats: чатов вне списка не существует. " +
	"Отправка — только через черновик и подтверждение человека " +
	"(кроме чатов с автоотправкой — там есть окно на отмену)."

func ptr[T any](v T) *T { return &v }

var (
	readOnly    = &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)}
	destructive = &mcp.ToolAnnotations{DestructiveHint: ptr(true)}
)

// ErrorKind — как ошибка выглядит для агента (см. «Типичные ошибки» в AGENTS.md).
func ErrorKind(err error) string {
	var (
		cfgErr    *config.Error
		cfgDenied *config.Denied
		denied    *core.Denied
		refused   *attach.Refused
		notLogged *tgc.NotLoggedIn
		notFound  *outbox.NotFound
		bad       *core.Bad
		tgBad     *tgc.Bad
		attBad    *attach.Bad
		badState  *outbox.BadState
		busy      *lock.Busy
		coreNF    *core.NotFound
		remote    *svc.Error
	)
	switch {
	case errors.As(err, &remote) && remote.Kind != "":
		return remote.Kind // из службы другого аккаунта — как назвала она
	case errors.As(err, &cfgErr):
		return "config"
	case errors.As(err, &cfgDenied), errors.As(err, &denied), errors.As(err, &refused):
		return "denied"
	case errors.As(err, &notLogged):
		return "not_logged_in"
	case errors.As(err, &notFound), errors.As(err, &coreNF):
		return "not_found"
	case errors.As(err, &bad), errors.As(err, &tgBad), errors.As(err, &attBad), errors.As(err, &badState):
		return "bad_request"
	case errors.As(err, &busy), errors.As(err, new(tgc.ServiceOwned)):
		return "SessionBusy"
	case errors.Is(err, context.DeadlineExceeded):
		return "TimeoutError"
	}
	if tgc.RPCMessage(err) != "" {
		return "RPCError"
	}
	return "error"
}

func errorText(err error) string {
	return omap.Pretty(omap.New().Set("error", ErrorKind(err)).Set("message", err.Error()))
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// run — единая точка входа: конфиг, вызов, человекочитаемая ошибка вместо трейса.
func run(ctx context.Context, limit time.Duration, fn func(ctx context.Context, s *config.Settings) (*omap.Map, error)) (*mcp.CallToolResult, any, error) {
	s, err := load(ctx)
	if err != nil {
		return text(errorText(err)), nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	out, err := safe(ctx, s, fn)
	if err != nil {
		return text(errorText(err)), nil, nil
	}
	return text(omap.Pretty(out)), nil, nil
}

func safe(ctx context.Context, s *config.Settings, fn func(ctx context.Context, s *config.Settings) (*omap.Map, error)) (out *omap.Map, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("внутренняя ошибка: %v", r)
		}
	}()
	return fn(ctx, s)
}

func localPaths(doc string) string {
	return strings.NewReplacer("<DATA>", config.DataPath(), "<TG>", config.CLIHint()).Replace(doc)
}

// ── входные параметры инструментов ─────────────────────────────────────

type listChatsIn struct {
	WithStatus bool `json:"with_status,omitempty" jsonschema:"ещё и непрочитанные и превью последнего сообщения"`
}

type readChatIn struct {
	Chat     string `json:"chat" jsonschema:"alias из tg_list_chats"`
	Limit    int    `json:"limit,omitempty" jsonschema:"сколько сообщений (по умолчанию 50)"`
	BeforeID int    `json:"before_id,omitempty" jsonschema:"вернуть сообщения СТАРШЕ этого id"`
	AfterID  int    `json:"after_id,omitempty" jsonschema:"вернуть сообщения НОВЕЕ этого id"`
	IDs      []int  `json:"ids,omitempty" jsonschema:"конкретные сообщения по id (тогда limit, before_id, after_id не нужны)"`
	Topic    int    `json:"topic,omitempty" jsonschema:"только эта тема форума (id из tg_list_topics; General — 1)"`
}

type topicsIn struct {
	Chat  string `json:"chat" jsonschema:"alias из tg_list_chats"`
	Limit int    `json:"limit,omitempty" jsonschema:"сколько тем (по умолчанию 100)"`
}

type foldersIn struct {
	Account string `json:"account,omitempty" jsonschema:"аккаунт из tg_list_chats, если их несколько (по умолчанию основной)"`
}

type folderChatsIn struct {
	Folder  string `json:"folder" jsonschema:"id или название папки из tg_list_folders"`
	Account string `json:"account,omitempty" jsonschema:"аккаунт, чьи папки (по умолчанию основной)"`
}

type accountNoteIn struct {
	Account  string `json:"account,omitempty" jsonschema:"аккаунт из tg_list_chats (по умолчанию основной)"`
	Text     string `json:"text,omitempty" jsonschema:"текст заметки"`
	From     string `json:"from,omitempty" jsonschema:"подпись: из какого ты проекта или задачи"`
	DeleteID string `json:"delete_id,omitempty" jsonschema:"убрать свою заметку с этим id (тогда text не нужен)"`
}

type requestAccessIn struct {
	Account string `json:"account,omitempty" jsonschema:"аккаунт, чей чат (по умолчанию основной)"`
	ChatID  int64  `json:"chat_id" jsonschema:"id чата из tg_folder_chats"`
	Send    bool   `json:"send,omitempty" jsonschema:"нужна ещё и отправка (по умолчанию — только чтение)"`
	Reason  string `json:"reason" jsonschema:"зачем чат и для какого проекта — владелец решает по этому"`
}

type waitAccessIn struct {
	RequestID  string `json:"request_id" jsonschema:"id запроса из tg_request_access"`
	TimeoutSec int    `json:"timeout_sec,omitempty" jsonschema:"сколько ждать, секунд (по умолчанию 300)"`
}

type subscribeIn struct {
	Chat    string `json:"chat" jsonschema:"alias из tg_list_chats"`
	AfterID int    `json:"after_id,omitempty" jsonschema:"последний обработанный id: о более новых сообщат сразу"`
	Topic   int    `json:"topic,omitempty" jsonschema:"только эта тема форума (0 — все темы)"`
}

type unsubscribeIn struct {
	Chat string `json:"chat,omitempty" jsonschema:"alias; без него — все подписки сессии"`
}

type msgIn struct {
	Chat      string `json:"chat" jsonschema:"alias из tg_list_chats"`
	MessageID int    `json:"message_id" jsonschema:"id сообщения из tg_read_chat"`
}

type searchIn struct {
	Chat  string `json:"chat" jsonschema:"alias из tg_list_chats"`
	Query string `json:"query" jsonschema:"что искать"`
	Limit int    `json:"limit,omitempty" jsonschema:"сколько сообщений (по умолчанию 30)"`
	Topic int    `json:"topic,omitempty" jsonschema:"искать только в этой теме форума"`
}

type draftIn struct {
	Chat    string   `json:"chat" jsonschema:"alias из tg_list_chats"`
	Text    string   `json:"text,omitempty" jsonschema:"текст (Markdown по умолчанию)"`
	ReplyTo *int64   `json:"reply_to,omitempty" jsonschema:"id сообщения, на которое отвечаем"`
	Note    string   `json:"note,omitempty" jsonschema:"зачем это сообщение и из какого проекта"`
	Format  string   `json:"format,omitempty" jsonschema:"markdown (по умолчанию) или plain"`
	Files   []string `json:"files,omitempty" jsonschema:"локальные пути к файлам, до 10 штук"`
	AsFiles bool     `json:"as_files,omitempty" jsonschema:"картинки тоже файлами, без сжатия (по умолчанию jpg/png уходят фото)"`
	Topic   int      `json:"topic,omitempty" jsonschema:"тема форума, куда писать (id из tg_list_topics); при reply_to — по исходному сообщению"`
}

type reactIn struct {
	Chat      string `json:"chat" jsonschema:"alias из tg_list_chats"`
	MessageID int    `json:"message_id" jsonschema:"id сообщения"`
	Emoji     string `json:"emoji,omitempty" jsonschema:"одна стандартная реакция (по умолчанию 👍)"`
	Remove    bool   `json:"remove,omitempty" jsonschema:"снять реакцию владельца"`
}

type waitIn struct {
	DraftID    string `json:"draft_id" jsonschema:"id черновика"`
	TimeoutSec int    `json:"timeout_sec,omitempty" jsonschema:"сколько ждать, секунд (по умолчанию 300)"`
}

type sendIn struct {
	DraftID          string `json:"draft_id" jsonschema:"id черновика"`
	UserConfirmation string `json:"user_confirmation,omitempty" jsonschema:"дословный ответ пользователя (agent_confirm)"`
}

type listDraftsIn struct {
	Status string `json:"status,omitempty" jsonschema:"pending, approved, scheduled, sent, cancelled, expired"`
}

type draftIDIn struct {
	DraftID string `json:"draft_id" jsonschema:"id черновика"`
}

// New собирает сервер со всеми инструментами.
func New() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "telegram", Version: "0.2.0"}, &mcp.ServerOptions{Instructions: instructions})
	server.AddReceivingMiddleware(route) // вызовы к чатам другого аккаунта — в его службу

	mcp.AddTool(server, &mcp.Tool{Name: "tg_list_chats", Annotations: readOnly, Description: localPaths(`Список чатов Telegram, к которым у тебя есть доступ (белый список).

Других чатов для тебя не существует: попытка обратиться к чату вне списка
вернёт ошибку. В ответе у каждого чата: alias (им и обращайся),
can_read / can_send, auto_send и заметка владельца.

auto_send = true — в этом чате сообщение уходит без кнопки: после
tg_draft_message есть окно (обычно 30 с), чтобы отменить через
tg_cancel_draft, потом оно отправляется само.

with_status=true дополнительно тянет число непрочитанных и превью
последнего сообщения (медленнее, один запрос к Telegram).

У читаемых чатов есть feed: path — локальный файл, куда служба сразу по
приходу дописывает id новых сообщений (по одному на строку), last_id —
последний из них. Сравни last_id со своим последним обработанным id и,
если он больше, забери новое через tg_read_chat(after_id=<твой последний id>).

Чтобы узнавать о новых сообщениях сразу, подпишись: tg_subscribe — служба
сама разбудит эту сессию, когда в чате появится сообщение (подробно в
AGENTS.md, «Как следить за чатом»).

Если аккаунтов Telegram у владельца несколько, ответ разбит по ним:
accounts[] — у каждого account (имя), user (чей это аккаунт в Telegram),
notes (заметки владельца и агентов об аккаунте: от чьего имени там пишут,
каким тоном — учитывай их) и его chats. Чаты не основного аккаунта
называются «<аккаунт>/<alias>» — так и передавай их в chat остальных
инструментов. Черновики, подписки и запросы доступа работают так же.
Пока аккаунт один, ответ прежний; заметки к нему — в account_notes.`)},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listChatsIn) (*mcp.CallToolResult, any, error) {
			return run(ctx, 2*time.Minute, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				return core.ListChats(ctx, s, in.WithStatus)
			})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tg_account_note", Description: `Оставить заметку к аккаунту Telegram владельца (или убрать свою).

Заметки видны всем агентам в tg_list_chats (notes у аккаунта) и владельцу
в боте и окне управления. Пиши туда то, что пригодится другим агентам,
работающим с этим аккаунтом: чей он, от чьего имени и каким тоном там
пишут, о чём договорились с владельцем. Не пиши туда тексты переписки,
пароли и прочие секреты. Коротко и по делу; не дублируй уже записанное.

account   — имя аккаунта из tg_list_chats (по умолчанию основной)
text      — текст заметки
from      — подпись: из какого ты проекта или задачи
delete_id — убрать заметку с этим id (только заметки агентов; заметки
            владельца убирает он сам)`},
		func(ctx context.Context, _ *mcp.CallToolRequest, in accountNoteIn) (*mcp.CallToolResult, any, error) {
			return run(ctx, time.Minute, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				return core.AccountNoteTool(ctx, s, in.Account, in.Text, in.DeleteID, in.From)
			})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tg_list_folders", Annotations: readOnly, Description: `Папки Telegram владельца (как в приложении: «Работа», «Личное»…).

Нужно, чтобы найти чат, которого нет в tg_list_chats: смотришь папки,
потом чаты папки (tg_folder_chats) и просишь доступ (tg_request_access).
Аккаунтов несколько — account выбирает, чьи папки (по умолчанию основной);
тот же account передавай и в tg_folder_chats, и в tg_request_access.
В ответе: id, title, сколько чатов добавлено в папку явно и правила папки
(«группы», «контакты»…), если она собирает чаты по типу.`},
		func(ctx context.Context, _ *mcp.CallToolRequest, in foldersIn) (*mcp.CallToolResult, any, error) {
			return run(ctx, time.Minute, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				if err := checkAccount(in.Account); err != nil {
					return nil, err
				}
				return core.ListFolders(ctx, s)
			})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tg_folder_chats", Annotations: readOnly, Description: `Чаты в папке Telegram владельца: название, тип, непрочитанные.

Сообщений не показывает — только список. У каждого чата id и alias: alias
= null — чата нет в белом списке, читать и писать туда нельзя, пока
владелец не откроет (tg_request_access). access_request — по этому чату
уже ждёт запрос.`},
		func(ctx context.Context, _ *mcp.CallToolRequest, in folderChatsIn) (*mcp.CallToolResult, any, error) {
			return run(ctx, 3*time.Minute, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				if err := checkAccount(in.Account); err != nil {
					return nil, err
				}
				return core.FolderChats(ctx, s, in.Folder)
			})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tg_request_access", Description: `Попросить владельца открыть чат: карточка с кнопками уходит ему в бот.

chat_id — id из tg_folder_chats; send=true — нужна и отправка (каждое
сообщение всё равно пойдёт через черновик и подтверждение); reason —
зачем чат и для какого проекта, коротко и честно: владелец решает по нему.
Владелец может разрешить, разрешить только чтение или отказать. Разрешил —
чат появится в tg_list_chats с alias, и в эту сессию придёт уведомление.
Ждать явно — tg_wait_access(request_id). Не дублируй запрос и не проси
снова после отказа без новой просьбы владельца. Просить доступ стоит,
только когда чат действительно нужен для задачи владельца.`},
		func(ctx context.Context, _ *mcp.CallToolRequest, in requestAccessIn) (*mcp.CallToolResult, any, error) {
			return run(ctx, 2*time.Minute, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				if err := checkAccount(in.Account); err != nil {
					return nil, err
				}
				return core.RequestAccess(ctx, s, in.ChatID, true, in.Send, in.Reason)
			})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tg_wait_access", Description: `Дождаться решения владельца по запросу доступа (tg_request_access).

status: granted — чат открыт (alias, can_read, can_send внутри);
denied — отказ, не проси снова без новой просьбы владельца;
waiting — ещё не нажал: это не отказ, решение придёт уведомлением.`},
		func(ctx context.Context, _ *mcp.CallToolRequest, in waitAccessIn) (*mcp.CallToolResult, any, error) {
			timeout := in.TimeoutSec
			if timeout == 0 {
				timeout = 300
			}
			return run(ctx, time.Duration(timeout+60)*time.Second, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				return core.WaitAccess(ctx, s, in.RequestID, timeout)
			})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tg_subscribe", Description: localPaths(`Подписать эту сессию на новые сообщения разрешённого чата.

Когда в чате появится сообщение, служба tg-agent сама пришлёт в эту сессию
уведомление — даже если ты в этот момент ничего не делаешь: оно начнёт
новый ход. Опрашивать ленту, держать Monitor и перезапускать его не нужно.
Уведомление приходит как сообщение «от другой сессии» с подписью
tg-agent: в нём alias, сколько новых и готовый вызов
tg_read_chat(chat, after_id) — текст сообщений читай им. Изменили или
удалили уже известное тебе сообщение — придёт «изменены сообщения …» с
tg_read_chat(chat, ids=[…]) или «удалены сообщения …»: перечитай, ответ мог
устареть. Свои отправки и правки (агента и владельца) не будят.

chat     — alias из tg_list_chats (нужно can_read)
after_id — твой последний обработанный id: о том, что новее, сообщат
           сразу. Без него — только о сообщениях после подписки.
topic    — в чате-форуме: будить только о сообщениях этой темы. Без него
           уведомление скажет, в каких темах новое.

Подписка живёт, пока жива сессия, и переживает перезапуск службы и
возобновление сессии. Повторный вызов безопасен — он лишь переставляет
after_id. На несколько чатов — по вызову на каждый.
Отписаться — tg_unsubscribe.
Без службы или в старой сессии (до обновления tg-agent) вернёт ошибку —
тогда следи через Monitor и ` + "`<TG> watch`" + ` (AGENTS.md).
Агент в контейнере (этот сервер по HTTP): будится не твоя сессия, а тред
шлюза «TG: <чат>» в твоём окружении T3 — служба заводит его сама.`)},
		func(ctx context.Context, _ *mcp.CallToolRequest, in subscribeIn) (*mcp.CallToolResult, any, error) {
			return run(ctx, time.Minute, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				return core.Subscribe(ctx, s, in.Chat, in.AfterID, in.Topic)
			})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tg_unsubscribe", Description: `Снять подписку этой сессии на чат (tg_subscribe).

chat — alias; без него снимаются все подписки сессии.`},
		func(ctx context.Context, _ *mcp.CallToolRequest, in unsubscribeIn) (*mcp.CallToolResult, any, error) {
			return run(ctx, time.Minute, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				return core.Unsubscribe(ctx, s, in.Chat)
			})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tg_read_chat", Annotations: readOnly, Description: `Прочитать последние сообщения разрешённого чата.

chat      — alias из tg_list_chats
limit     — сколько сообщений (по умолчанию 50, потолок задан владельцем)
before_id — вернуть сообщения СТАРШЕ этого id (постраничная прокрутка назад)
after_id  — вернуть сообщения НОВЕЕ этого id (что нового с прошлого раза)
ids       — конкретные сообщения по id, например [3585092, 3585000]
topic     — только эта тема форума (id из tg_list_topics; General — 1)

Сообщения приходят в хронологическом порядке, старые сверху.
Чтение не помечает чат прочитанным.

Ответ на сообщение (reply_to), которого нет в выдаче, приходит вместе с ним:
reply_to_message — само исходное сообщение (у голосового — с расшифровкой);
null — оно удалено. Дальше по цепочке — через ids.

Чат-форум (с темами): в ответе forum = true, у каждого сообщения topic —
{id, title} темы, где оно лежит. Без topic приходят сообщения всех тем
вперемешку. Отвечаешь в теме — пиши черновик с reply_to (тема определится
сама) или с topic.

Голосовые и кружки (media = voice / video_note) приходят с расшифровкой
Telegram. transcript_status: done — transcript полный; pending — готово
только начало (transcript_partial), дожми через tg_transcribe;
not_requested — вызови tg_transcribe; error — скачай через
tg_download_file и расшифруй своим STT.`},
		func(ctx context.Context, _ *mcp.CallToolRequest, in readChatIn) (*mcp.CallToolResult, any, error) {
			return run(ctx, 3*time.Minute, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				return core.ReadChat(ctx, s, in.Chat, core.ReadOpts{Limit: in.Limit, BeforeID: in.BeforeID,
					AfterID: in.AfterID, IDs: in.IDs, Transcribe: true, Topic: in.Topic})
			})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tg_list_topics", Annotations: readOnly, Description: `Темы чата-форума (супергруппы с темами).

Чаты, где у сообщений в tg_read_chat есть topic, — форумы. Возвращает темы
с недавней активностью сверху: id (его передавай в topic у tg_read_chat,
tg_search_chat, tg_draft_message, tg_subscribe), title, unread,
last_message_id; closed — тема закрыта (писать туда может только админ),
pinned, hidden. У General id = 1. Если чат добавлен в белый список, доступны
все его темы.`},
		func(ctx context.Context, _ *mcp.CallToolRequest, in topicsIn) (*mcp.CallToolResult, any, error) {
			return run(ctx, 2*time.Minute, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				return core.ListTopics(ctx, s, in.Chat, in.Limit)
			})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tg_view_media", Annotations: readOnly, Description: `Посмотреть изображение из сообщения разрешённого чата.

Работает для media = photo, картинок-файлов (png, webp…) и стикеров;
у video / video_note / gif отдаёт превью-кадр. Возвращает описание
(kind, caption — подпись к фото) и саму картинку.

Альбом — это несколько сообщений с одинаковым grouped_id в tg_read_chat:
чтобы увидеть альбом целиком, смотри каждое. Не открывай все фото чата
подряд без нужды — только те, что важны для задачи.`},
		func(ctx context.Context, _ *mcp.CallToolRequest, in msgIn) (*mcp.CallToolResult, any, error) {
			s, err := load(ctx)
			if err != nil {
				return text(errorText(err)), nil, nil
			}
			ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			meta, img, err := core.ViewMedia(ctx, s, in.Chat, in.MessageID)
			if err != nil {
				return text(errorText(err)), nil, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: omap.Pretty(meta)},
				&mcp.ImageContent{Data: img.Data, MIMEType: "image/" + img.Format},
			}}, nil, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tg_download_file", Annotations: readOnly, Description: localPaths(`Скачать вложение из сообщения разрешённого чата целиком.

Годится для любого файла: документы, видео (целиком, не превью), кружки,
голосовые (.oga, Opus — годится для whisper и других STT), аудио, фото в
исходном качестве. Сохраняет в
<DATA>\downloads\<alias>\ и возвращает path —
дальше открывай его своими инструментами (Read, ffmpeg, архиватор…).
Повторный вызов не качает заново. У сообщений с файлом в tg_read_chat
есть поле file (name, size, mime_type).
Чтобы просто посмотреть картинку, удобнее tg_view_media.`)},
		func(ctx context.Context, _ *mcp.CallToolRequest, in msgIn) (*mcp.CallToolResult, any, error) {
			return run(ctx, 60*time.Minute, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				return core.DownloadFile(ctx, s, in.Chat, in.MessageID)
			})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tg_transcribe", Annotations: readOnly, Description: `Расшифровать голосовое или кружок из разрешённого чата.

Использует встроенную расшифровку Telegram (как кнопка «→A» в
приложении) и ждёт ПОЛНЫЙ текст — Telegram отдаёт его частями, до
финала может пройти до пары минут на длинной записи. Нужен, когда в
tg_read_chat transcript_status = pending или not_requested.
Записи от ~5 минут Telegram не расшифровывает (status = error,
MSG_VOICE_TOO_LONG) — тогда скачай tg_download_file и прогони через STT.
Расшифровка автоматическая: имена, термины и цифры могут быть искажены.`},
		func(ctx context.Context, _ *mcp.CallToolRequest, in msgIn) (*mcp.CallToolResult, any, error) {
			return run(ctx, 7*time.Minute, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				return core.TranscribeMessage(ctx, s, in.Chat, in.MessageID)
			})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tg_search_chat", Annotations: readOnly,
		Description: "Поиск по тексту сообщений внутри одного разрешённого чата."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, any, error) {
			limit := in.Limit
			if limit == 0 {
				limit = 30
			}
			return run(ctx, 3*time.Minute, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				return core.ReadChat(ctx, s, in.Chat, core.ReadOpts{Limit: limit, Search: in.Query, Transcribe: true, Topic: in.Topic})
			})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tg_draft_message", Description: `Подготовить сообщение к отправке. САМО ПО СЕБЕ НЕ ОТПРАВЛЯЕТ — кроме чатов с автоотправкой.

В обычном режиме (` + "`bot_approval`" + `) карточка с текстом сразу уходит владельцу
в бот подтверждений, где под ней две кнопки. Дальше вызывай
tg_wait_approval — нажатие «Отправить» отправит сообщение само.
Дублировать текст в чат владельцу не нужно, он видит его в карточке;
достаточно одной строки о том, что отправил на подтверждение.

В чате с auto_send = true (см. tg_list_chats) кнопки нет: ответ придёт со
status = scheduled и send_at — сообщение УЙДЁТ САМО в это время. До него
можно отменить через tg_cancel_draft. Поэтому пиши туда только то, что
точно должно уйти.

text — по умолчанию Markdown, Telegram отрисует его сам: **жирный**, *курсив*,
` + "`код`" + `, # заголовки, - списки, | таблицы |, > цитаты, --- разделитель,
` + "```блоки кода```" + `, [ссылки](https://...). Пиши как для человека в
мессенджере: коротко, без лишних заголовков в двухстрочном ответе.
format="plain" — отправить текст как есть, без разметки.
До 32768 символов.

files — локальные пути к файлам (до 10 штук), уходят следом за текстом.
Текст при этом можно не писать. Картинки jpg/png уходят ФОТО — видны прямо
в ленте чата (Telegram их пережмёт); остальные файлы — документами без
пережатия. Фото и документы приходят двумя альбомами. В ответе у каждого
файла as = photo / document. Нужна картинка в исходном качестве, файлом —
as_files=true. Картинку не надо выкладывать по ссылке, чтобы её было видно:
достаточно передать её в files. Файлы копируются в момент вызова: владелец
одобряет и получает ровно эту версию, правки после вызова не попадут —
нужен новый черновик. Не принимаются файлы самого tg-agent, .env, ключи,
сессии и прочие секреты. Агенту в контейнере (этот сервер по HTTP) files
недоступны: пути указывали бы на диск шлюза.

note — зачем это сообщение и из какого проекта; показывается в карточке,
помогает человеку решить, не переспрашивая.
reply_to — id сообщения, на которое отвечаем (из tg_read_chat).
topic — в чате-форуме: тема, куда писать (id из tg_list_topics). С reply_to
указывать не нужно: ответ уходит в тему исходного сообщения. Без того и
другого сообщение форума уходит в General. В ответе — topic {id, title}:
проверь, что тема та.

В режимах human_approval / agent_confirm смотри поле next_step из ответа.`},
		func(ctx context.Context, _ *mcp.CallToolRequest, in draftIn) (*mcp.CallToolResult, any, error) {
			return run(ctx, 10*time.Minute, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				return core.DraftMessage(ctx, s, in.Chat, in.Text, in.ReplyTo, in.Note, in.Format, in.Files, in.AsFiles, in.Topic)
			})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tg_react", Description: `Поставить реакцию на сообщение от имени владельца (или снять свою).

Работает сразу, без карточки на одобрение, и только в чатах с
can_send = true. Реакция видна всем в чате — ставь её, только когда
владелец об этом попросил или это явно часть поручения (например,
«отметь 👍, что задачу принял»). Не ставь реакции «от себя» ради
вежливости.

emoji — одна стандартная реакция Telegram. Сейчас доступны (проверено по API):
❤ 👍 👎 🔥 🥰 👏 😁 🤔 🤯 😱 🤬 😢 🎉 🤩 🤮 💩 🙏 👌 🕊 🤡 🥱 🥴 😍 🐳 ❤‍🔥 🌚 🌭
💯 🤣 ⚡ 🍌 🏆 💔 🤨 😐 🍓 🍾 💋 🖕 😈 😴 😭 🤓 👻 👨‍💻 👀 🎃 🙈 😇 😨 🤝 ✍ 🤗 🫡
🎅 🎄 ☃ 💅 🤪 🗿 🆒 💘 🙉 🦄 😘 💊 🙊 😎 👾 🤷‍♂ 🤷 🤷‍♀ 😡
Других нет: ✅ ❌ 👋 ⭐ реакциями не ставятся. «❤️» можно писать и так —
невидимый модификатор срезается сам.
Новая реакция заменяет прежнюю реакцию владельца на этом сообщении.
remove=true — снять реакцию владельца.
Текущие реакции на сообщениях видны в tg_read_chat (поле reactions,
mine=true — это реакция владельца).`},
		func(ctx context.Context, _ *mcp.CallToolRequest, in reactIn) (*mcp.CallToolResult, any, error) {
			emoji := in.Emoji
			if emoji == "" {
				emoji = "👍"
			}
			if in.Remove {
				emoji = ""
			}
			return run(ctx, 2*time.Minute, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				return core.React(ctx, s, in.Chat, in.MessageID, emoji)
			})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tg_wait_approval", Description: `Дождаться, пока человек нажмёт кнопку под карточкой в боте
(или пока уйдёт сообщение на автоотправке).

Возвращает status:
  sent      — сообщение ушло (message_id внутри);
  cancelled — владелец нажал «Отклонить»/«Отменить» или черновик отменили.
              Не пересоздавай черновик без новой просьбы;
  expired   — черновик протух, нужен новый;
  send_failed — владелец нажал «Отправить», но отправка сорвалась (error
              внутри). Черновик одобрен: повтори tg_send_draft(draft_id),
              заново спрашивать владельца не нужно;
  sending   — отправка ещё идёт;
  scheduled — автоотправка, время ещё не пришло;
  waiting   — человек ещё не нажал. Это НЕ отказ: карточка в боте живёт,
              нажатие сработает и позже. Скажи об этом и займись другим,
              не дёргай человека повторно.`},
		func(ctx context.Context, _ *mcp.CallToolRequest, in waitIn) (*mcp.CallToolResult, any, error) {
			timeout := in.TimeoutSec
			if timeout == 0 {
				timeout = 300
			}
			return run(ctx, time.Duration(timeout+180)*time.Second, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				return core.WaitApproval(ctx, s, in.DraftID, timeout)
			})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tg_send_draft", Annotations: destructive, Description: `Отправить ранее созданный черновик в Telegram.

В режиме bot_approval этот инструмент не нужен: отправку делает кнопка,
твоё дело — tg_wait_approval. Не нужен он и для автоотправки. Он для
остальных режимов:
  - human_approval — человек выполнил ` + "`tg approve <draft_id>`" + `;
  - agent_confirm  — человек согласился в диалоге, и ты передал его
    дословный ответ в user_confirmation.
Придумывать подтверждение за пользователя нельзя. Факт отправки и текст
попадают в аудит-лог владельца.`},
		func(ctx context.Context, _ *mcp.CallToolRequest, in sendIn) (*mcp.CallToolResult, any, error) {
			return run(ctx, 15*time.Minute, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				return core.SendDraft(ctx, s, in.DraftID, in.UserConfirmation)
			})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tg_list_drafts", Annotations: readOnly, Description: `Черновики и их статусы: pending, approved, scheduled, sent, cancelled, expired.

Полезно, чтобы проверить, подтвердил ли человек отправку.`},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listDraftsIn) (*mcp.CallToolResult, any, error) {
			return run(ctx, time.Minute, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				return core.ListDrafts(s, in.Status)
			})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tg_cancel_draft", Description: `Отменить черновик (например, пользователь передумал или правит текст).

Для автоотправки — единственный способ остановить сообщение, пока не
наступило send_at.`},
		func(ctx context.Context, _ *mcp.CallToolRequest, in draftIDIn) (*mcp.CallToolResult, any, error) {
			return run(ctx, 2*time.Minute, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				return core.CancelDraft(ctx, s, in.DraftID, "agent")
			})
		})

	return server
}

// Run — обслуживать MCP по stdio. stdout занят протоколом — логи только в stderr.
func Run(ctx context.Context) error {
	return New().Run(ctx, &mcp.StdioTransport{})
}

var _ = os.Stderr
