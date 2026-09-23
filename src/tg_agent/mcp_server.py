from __future__ import annotations

import json
import logging
import shutil
import sys

from mcp.server.mcpserver import Image, MCPServer
from mcp.types import ToolAnnotations

from . import service
from .client import NotLoggedIn
from .config import ROOT, ConfigError, load_settings


def _local_paths(fn):
    """Подставить в описание инструмента реальные пути этой установки: в
    репозитории остаются метки <ROOT>/<UV>, у каждого — свои пути."""
    uv = shutil.which("uv") or "uv"
    fn.__doc__ = (fn.__doc__ or "").replace("<ROOT>", str(ROOT)).replace("<UV>", uv)
    return fn

# stdio занят протоколом MCP — любые логи только в stderr
logging.basicConfig(stream=sys.stderr, level=logging.WARNING)
logging.getLogger("telethon").setLevel(logging.ERROR)

mcp = MCPServer(
    "telegram",
    instructions=(
        "Доступ к Telegram владельца, ограниченный белым списком чатов. "
        "Начинай с tg_list_chats: чатов вне списка не существует. "
        "Отправка — только через черновик и подтверждение человека."
    ),
)

READ_ONLY = ToolAnnotations(readOnlyHint=True, openWorldHint=True)


def _json(data: object) -> str:
    return json.dumps(data, ensure_ascii=False, indent=2)


async def _run(coro_factory) -> str:
    """Единая точка входа: конфиг, вызов, человекочитаемая ошибка вместо трейса."""
    try:
        settings = load_settings()
    except ConfigError as exc:
        return _json({"error": "config", "message": str(exc)})
    try:
        return _json(await coro_factory(settings))
    except PermissionError as exc:
        return _json({"error": "denied", "message": str(exc)})
    except NotLoggedIn as exc:
        return _json({"error": "not_logged_in", "message": str(exc)})
    except KeyError as exc:
        return _json({"error": "not_found", "message": str(exc)})
    except ValueError as exc:
        return _json({"error": "bad_request", "message": str(exc)})
    except Exception as exc:  # noqa: BLE001 — агенту нужен текст, а не падение сервера
        return _json({"error": type(exc).__name__, "message": str(exc)})


@mcp.tool(annotations=READ_ONLY)
@_local_paths
async def tg_list_chats(with_status: bool = False) -> str:
    """Список чатов Telegram, к которым у тебя есть доступ (белый список).

    Других чатов для тебя не существует: попытка обратиться к чату вне списка
    вернёт ошибку. В ответе у каждого чата: alias (им и обращайся),
    can_read / can_send и заметка владельца.

    with_status=True дополнительно тянет число непрочитанных и превью
    последнего сообщения (медленнее, один запрос к Telegram).

    У читаемых чатов есть feed: path — локальный файл, куда демон раз в
    ~10 с дописывает id новых сообщений (по одному на строку), last_id —
    последний из них. Так следят за чатом без опроса Telegram: сравни
    last_id со своим последним обработанным id и, если он больше, забери
    новое через tg_read_chat(after_id=<твой последний id>). Подписка с
    уведомлениями: запусти через Monitor команду
    `<UV> run --directory <ROOT> tg watch <alias>`
    — она печатает «alias id» на каждое новое сообщение.
    """
    return await _run(lambda s: service.list_chats(s, with_status=with_status))


@mcp.tool(annotations=READ_ONLY)
async def tg_read_chat(
    chat: str,
    limit: int = 50,
    before_id: int | None = None,
    after_id: int | None = None,
) -> str:
    """Прочитать последние сообщения разрешённого чата.

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
    tg_download_file и расшифруй своим STT.
    """
    return await _run(
        lambda s: service.read_chat(s, chat, limit=limit, before_id=before_id, after_id=after_id)
    )


@mcp.tool(annotations=READ_ONLY, structured_output=False)
async def tg_view_media(chat: str, message_id: int) -> list:
    """Посмотреть изображение из сообщения разрешённого чата.

    Работает для media = photo, картинок-файлов (png, webp…) и стикеров;
    у video / video_note / gif отдаёт превью-кадр. Возвращает описание
    (kind, caption — подпись к фото) и саму картинку.

    Альбом — это несколько сообщений с одинаковым grouped_id в tg_read_chat:
    чтобы увидеть альбом целиком, смотри каждое. Не открывай все фото чата
    подряд без нужды — только те, что важны для задачи.
    """
    holder: dict = {}

    async def go(s):
        meta, data, fmt = await service.view_media(s, chat, message_id)
        holder["image"] = Image(data=data, format=fmt)
        return meta

    text = await _run(go)
    return [text, holder["image"]] if "image" in holder else [text]


@mcp.tool(annotations=READ_ONLY)
@_local_paths
async def tg_download_file(chat: str, message_id: int) -> str:
    """Скачать вложение из сообщения разрешённого чата целиком.

    Годится для любого файла: документы, видео (целиком, не превью), кружки,
    голосовые (.oga, Opus — годится для whisper и других STT), аудио, фото в
    исходном качестве. Сохраняет в
    <ROOT>/data/downloads/<alias>/ и возвращает path —
    дальше открывай его своими инструментами (Read, ffmpeg, архиватор…).
    Повторный вызов не качает заново. У сообщений с файлом в tg_read_chat
    есть поле file (name, size, mime_type).
    Чтобы просто посмотреть картинку, удобнее tg_view_media.
    """
    return await _run(lambda s: service.download_file(s, chat, message_id))


@mcp.tool(annotations=READ_ONLY)
async def tg_transcribe(chat: str, message_id: int) -> str:
    """Расшифровать голосовое или кружок из разрешённого чата.

    Использует встроенную расшифровку Telegram (как кнопка «→A» в
    приложении) и ждёт ПОЛНЫЙ текст — Telegram отдаёт его частями, до
    финала может пройти до пары минут на длинной записи. Нужен, когда в
    tg_read_chat transcript_status = pending или not_requested.
    Записи от ~5 минут Telegram не расшифровывает (status = error,
    MSG_VOICE_TOO_LONG) — тогда скачай tg_download_file и прогони через STT.
    Расшифровка автоматическая: имена, термины и цифры могут быть искажены.
    """
    return await _run(lambda s: service.transcribe_message(s, chat, message_id))


@mcp.tool(annotations=READ_ONLY)
async def tg_search_chat(chat: str, query: str, limit: int = 30) -> str:
    """Поиск по тексту сообщений внутри одного разрешённого чата."""
    return await _run(lambda s: service.read_chat(s, chat, limit=limit, search=query))


@mcp.tool()
async def tg_draft_message(
    chat: str,
    text: str = "",
    reply_to: int | None = None,
    note: str = "",
    format: str = "markdown",
    files: list[str] | None = None,
) -> str:
    """Подготовить сообщение к отправке. НИЧЕГО НЕ ОТПРАВЛЯЕТ.

    В обычном режиме (`bot_approval`) карточка с текстом сразу уходит владельцу
    в бот подтверждений, где под ней две кнопки. Дальше вызывай
    tg_wait_approval — нажатие «Отправить» отправит сообщение само.
    Дублировать текст в чат владельцу не нужно, он видит его в карточке;
    достаточно одной строки о том, что отправил на подтверждение.

    text — по умолчанию Markdown, Telegram отрисует его сам: **жирный**, *курсив*,
    `код`, # заголовки, - списки, | таблицы |, > цитаты, --- разделитель,
    ```блоки кода```, [ссылки](https://...). Пиши как для человека в
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

    В режимах human_approval / agent_confirm смотри поле next_step из ответа.
    """
    return await _run(
        lambda s: service.draft_message(
            s, chat, text, reply_to=reply_to, note=note, fmt=format, files=files
        )
    )


@mcp.tool()
async def tg_react(chat: str, message_id: int, emoji: str = "👍", remove: bool = False) -> str:
    """Поставить реакцию на сообщение от имени владельца (или снять свою).

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
    remove=True — снять реакцию владельца.
    Текущие реакции на сообщениях видны в tg_read_chat (поле reactions,
    mine=true — это реакция владельца).
    """
    return await _run(lambda s: service.react(s, chat, message_id, None if remove else emoji))


@mcp.tool()
async def tg_wait_approval(draft_id: str, timeout_sec: int = 300) -> str:
    """Дождаться, пока человек нажмёт кнопку под карточкой в боте.

    Возвращает status:
      sent      — человек нажал «Отправить», сообщение ушло (message_id внутри);
      cancelled — нажал «Отклонить». Не пересоздавай черновик без новой просьбы;
      expired   — черновик протух, нужен новый;
      waiting   — человек ещё не нажал. Это НЕ отказ: карточка в боте живёт,
                  нажатие сработает и позже. Скажи об этом и займись другим,
                  не дёргай человека повторно.
    """
    return await _run(lambda s: service.wait_approval(s, draft_id, timeout_sec))


@mcp.tool(annotations=ToolAnnotations(readOnlyHint=False, destructiveHint=True, idempotentHint=False))
async def tg_send_draft(draft_id: str, user_confirmation: str = "") -> str:
    """Отправить ранее созданный черновик в Telegram.

    В режиме bot_approval этот инструмент не нужен: отправку делает кнопка,
    твоё дело — tg_wait_approval. Он для остальных режимов:
      - human_approval — человек выполнил `tg approve <draft_id>`;
      - agent_confirm  — человек согласился в диалоге, и ты передал его
        дословный ответ в user_confirmation.
    Придумывать подтверждение за пользователя нельзя. Факт отправки и текст
    попадают в аудит-лог владельца.
    """
    return await _run(lambda s: service.send_draft(s, draft_id, user_confirmation=user_confirmation))


@mcp.tool(annotations=READ_ONLY)
async def tg_list_drafts(status: str | None = None) -> str:
    """Черновики и их статусы: pending, approved, sent, cancelled, expired.

    Полезно, чтобы проверить, подтвердил ли человек отправку.
    """
    return await _run(lambda s: service.list_drafts(s, status))


@mcp.tool()
async def tg_cancel_draft(draft_id: str) -> str:
    """Отменить черновик (например, пользователь передумал или правит текст)."""
    return await _run(lambda s: service.cancel_draft(s, draft_id, by="agent"))


def main() -> None:
    mcp.run()


if __name__ == "__main__":
    main()
