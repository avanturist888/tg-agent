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
	)
	switch {
	case errors.As(err, &cfgErr):
		return "config"
	case errors.As(err, &cfgDenied), errors.As(err, &denied), errors.As(err, &refused):
		return "denied"
	case errors.As(err, &notLogged):
		return "not_logged_in"
	case errors.As(err, &notFound):
		return "not_found"
	case errors.As(err, &bad), errors.As(err, &tgBad), errors.As(err, &attBad), errors.As(err, &badState):
		return "bad_request"
	case errors.As(err, &busy):
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
	s, err := config.Load()
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
	return strings.NewReplacer("<ROOT>", config.Root, "<TG>", config.CLIHint()).Replace(doc)
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
}

type msgIn struct {
	Chat      string `json:"chat" jsonschema:"alias из tg_list_chats"`
	MessageID int    `json:"message_id" jsonschema:"id сообщения из tg_read_chat"`
}

type searchIn struct {
	Chat  string `json:"chat" jsonschema:"alias из tg_list_chats"`
	Query string `json:"query" jsonschema:"что искать"`
	Limit int    `json:"limit,omitempty" jsonschema:"сколько сообщений (по умолчанию 30)"`
}

type draftIn struct {
	Chat    string   `json:"chat" jsonschema:"alias из tg_list_chats"`
	Text    string   `json:"text,omitempty" jsonschema:"текст (Markdown по умолчанию)"`
	ReplyTo *int64   `json:"reply_to,omitempty" jsonschema:"id сообщения, на которое отвечаем"`
	Note    string   `json:"note,omitempty" jsonschema:"зачем это сообщение и из какого проекта"`
	Format  string   `json:"format,omitempty" jsonschema:"markdown (по умолчанию) или plain"`
	Files   []string `json:"files,omitempty" jsonschema:"локальные пути к файлам, до 10 штук"`
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

	mcp.AddTool(server, &mcp.Tool{Name: "tg_list_chats", Annotations: readOnly, Description: localPaths(`Список чатов Telegram, к которым у тебя есть доступ (белый список).

Других чатов для тебя не существует: попытка обратиться к чату вне списка
вернёт ошибку. В ответе у каждого чата: alias (им и обращайся),
can_read / can_send, auto_send и заметка владельца.

auto_send = true — в этом чате сообщение уходит без кнопки: после
tg_draft_message есть окно (обычно 30 с), чтобы отменить через
tg_cancel_draft, потом оно отправляется само.

with_status=true дополнительно тянет число непрочитанных и превью
последнего сообщения (медленнее, один запрос к Telegram).

У читаемых чатов есть feed: path — локальный файл, куда демон раз в
~10 с дописывает id новых сообщений (по одному на строку), last_id —
последний из них. Так следят за чатом без опроса Telegram: сравни
last_id со своим последним обработанным id и, если он больше, забери
новое через tg_read_chat(after_id=<твой последний id>). Подписка с
уведомлениями: запусти через Monitor команду
` + "`<TG> watch <alias>`" + `
— она печатает «alias id» на каждое новое сообщение.`)},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listChatsIn) (*mcp.CallToolResult, any, error) {
			return run(ctx, 2*time.Minute, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				return core.ListChats(ctx, s, in.WithStatus)
			})
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tg_read_chat", Annotations: readOnly, Description: `Прочитать последние сообщения разрешённого чата.

chat      — alias из tg_list_chats
limit     — сколько сообщений (по умолчанию 50, потолок задан владельцем)
before_id — вернуть сообщения СТАРШЕ этого id (постраничная прокрутка назад)
after_id  — вернуть сообщения НОВЕЕ этого id (что нового с прошлого раза)

Сообщения приходят в хронологическом порядке, старые сверху.
Чтение не помечает чат прочитанным.

Голосовые и кружки (media = voice / video_note) приходят с расшифровкой
Telegram. transcript_status: done — transcript полный; pending — готово
только начало (transcript_partial), дожми через tg_transcribe;
not_requested — вызови tg_transcribe; error — скачай через
tg_download_file и расшифруй своим STT.`},
		func(ctx context.Context, _ *mcp.CallToolRequest, in readChatIn) (*mcp.CallToolResult, any, error) {
			return run(ctx, 3*time.Minute, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				return core.ReadChat(ctx, s, in.Chat, in.Limit, in.BeforeID, in.AfterID, "", true)
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
			s, err := config.Load()
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
<ROOT>\data\downloads\<alias>\ и возвращает path —
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
				return core.ReadChat(ctx, s, in.Chat, limit, 0, 0, in.Query, true)
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

files — локальные пути к файлам (до 10 штук), уходят документами без
пережатия следом за текстом. Текст при этом можно не писать. Файлы
копируются в момент вызова: владелец одобряет и получает ровно эту версию,
правки после вызова не попадут — нужен новый черновик. Не принимаются
файлы самого tg-agent, .env, ключи, сессии и прочие секреты.

note — зачем это сообщение и из какого проекта; показывается в карточке,
помогает человеку решить, не переспрашивая.
reply_to — id сообщения, на которое отвечаем (из tg_read_chat).

В режимах human_approval / agent_confirm смотри поле next_step из ответа.`},
		func(ctx context.Context, _ *mcp.CallToolRequest, in draftIn) (*mcp.CallToolResult, any, error) {
			return run(ctx, 10*time.Minute, func(ctx context.Context, s *config.Settings) (*omap.Map, error) {
				return core.DraftMessage(ctx, s, in.Chat, in.Text, in.ReplyTo, in.Note, in.Format, in.Files)
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
