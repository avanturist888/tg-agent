from __future__ import annotations

import asyncio
import json
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any

from telethon import TelegramClient, errors, events
from telethon.tl import functions, types

from .config import ROOT, ChatRule, Settings
from . import richtext
from .lock import FileLock


class NotLoggedIn(RuntimeError):
    pass


@asynccontextmanager
async def open_client(settings: Settings, *, require_auth: bool = True, wait: float = 45.0):
    """Подключиться на время запроса и сразу отпустить сессию.

    Долго держать соединение нельзя: параллельно живут MCP-серверы других
    проектов, а файл сессии один.
    """
    # wait — сколько ждать, пока сессию отпустит другой процесс tg-agent
    with FileLock(settings.session_path, timeout=wait):
        client = TelegramClient(
            str(settings.session_path),
            settings.api_id,
            settings.api_hash,
            proxy=settings.proxy,
            device_model="tg-agent",
            system_version="claude-code",
            app_version="0.1.0",
        )
        await client.connect()
        try:
            if require_auth and not await client.is_user_authorized():
                raise NotLoggedIn(
                    "Нет валидной сессии Telegram. Человек должен выполнить "
                    f"`uv run --directory {ROOT} tg login` и ввести код из Telegram."
                )
            yield client
        finally:
            await client.disconnect()


async def entity_for(client: TelegramClient, rule: ChatRule):
    peer: Any = rule.peer
    if isinstance(peer, str) and peer.strip().lower() == "me":
        return "me"
    try:
        return await client.get_entity(peer)
    except (ValueError, errors.RPCError):
        # id есть, но его нет в локальном кэше сессии — прогреваем диалогами
        await client.get_dialogs(limit=200)
        return await client.get_entity(peer)


def _media_label(msg) -> str | None:
    if msg.photo:
        return "photo"
    if msg.voice:
        return "voice"
    if msg.video_note:
        return "video_note"
    if msg.video:
        return "video"
    if msg.audio:
        return "audio"
    if msg.sticker:
        return "sticker"
    if msg.poll:
        return "poll"
    if msg.document:
        name = getattr(msg.file, "name", None)
        return f"file:{name}" if name else "document"
    if isinstance(msg.media, types.MessageMediaWebPage):
        return "link_preview"
    if msg.media is not None:
        return type(msg.media).__name__
    return None


def _person(entity) -> dict | None:
    if entity is None:
        return None
    name = " ".join(
        p for p in [getattr(entity, "first_name", None), getattr(entity, "last_name", None)] if p
    ).strip()
    return {
        "id": getattr(entity, "id", None),
        "name": name or getattr(entity, "title", None) or "?",
        "username": getattr(entity, "username", None),
        "bot": bool(getattr(entity, "bot", False)),
    }


async def _serialize(msg) -> dict:
    sender = None
    try:
        sender = _person(await msg.get_sender())
    except Exception:  # noqa: BLE001 — удалённый аккаунт или канал без автора
        sender = {"id": msg.sender_id, "name": "?", "username": None, "bot": False}
    out: dict[str, Any] = {
        "id": msg.id,
        "date": msg.date.isoformat() if msg.date else None,
        "from": sender,
        "text": msg.message or "",
    }
    rich = getattr(msg, "rich_message", None)
    if rich is not None and not out["text"]:
        # у rich-сообщения message пустой — текст собираем из блоков
        out["text"] = richtext.to_markdown(rich)
        out["rich"] = True
    if msg.reply_to_msg_id:
        out["reply_to"] = msg.reply_to_msg_id
    media = _media_label(msg)
    if media:
        out["media"] = media
    if msg.file and not msg.photo and not msg.sticker:
        out["file"] = {"name": msg.file.name, "size": msg.file.size, "mime_type": msg.file.mime_type}
    if media in ("voice", "video_note", "video", "audio") and getattr(msg.file, "duration", None):
        out["duration"] = round(msg.file.duration, 1)  # секунды
    if msg.grouped_id:
        out["grouped_id"] = msg.grouped_id  # одинаковый у всех фото одного альбома
    reactions = _reactions(msg)
    if reactions:
        out["reactions"] = reactions
    if msg.edit_date:
        out["edited"] = msg.edit_date.isoformat()
    if getattr(msg, "action", None):
        out["service_action"] = type(msg.action).__name__
    return out


def _reaction_label(reaction) -> str:
    if isinstance(reaction, types.ReactionEmoji):
        return reaction.emoticon
    if isinstance(reaction, types.ReactionCustomEmoji):
        return f"custom:{reaction.document_id}"
    if isinstance(reaction, types.ReactionPaid):
        return "⭐"
    return type(reaction).__name__


def _reactions(msg) -> list[dict] | None:
    results = getattr(getattr(msg, "reactions", None), "results", None)
    if not results:
        return None
    return [
        {"emoji": _reaction_label(r.reaction), "count": r.count, "mine": r.chosen_order is not None}
        for r in results
    ]


def normalize_emoji(emoji: str) -> str:
    """Telegram хранит реакции без селектора вариации: «❤», а не «❤️».
    Агенты обычно пишут с ним — без нормализации сервер отвечает REACTION_INVALID."""
    return emoji.replace("️", "").strip()


async def available_reactions(client: TelegramClient) -> list[str]:
    """Активные стандартные реакции — живой список от Telegram."""
    res = await client(functions.messages.GetAvailableReactionsRequest(hash=0))
    return [r.reaction for r in getattr(res, "reactions", []) if not r.inactive]


async def set_reaction(client: TelegramClient, entity, msg_id: int, emoji: str | None) -> list[dict] | None:
    """Поставить реакцию от аккаунта владельца (emoji=None — снять свою)."""
    if emoji:
        emoji = normalize_emoji(emoji)
        allowed = await available_reactions(client)
        if allowed and emoji not in allowed:
            raise ValueError(f"«{emoji}» нет среди реакций Telegram. Доступны: {' '.join(allowed)}")
    peer = await client.get_input_entity(entity)
    await client(
        functions.messages.SendReactionRequest(
            peer=peer,
            msg_id=msg_id,
            reaction=[types.ReactionEmoji(emoticon=emoji)] if emoji else [],
        )
    )
    msg = await client.get_messages(entity, ids=msg_id)
    return _reactions(msg) if msg else None


MAX_IMAGE_BYTES = 5 * 1024 * 1024  # потолок картинки для модели
IMAGE_FORMATS = {"image/jpeg": "jpeg", "image/png": "png", "image/webp": "webp", "image/gif": "gif"}


async def fetch_image(client: TelegramClient, entity, msg_id: int) -> dict:
    """Скачать изображение из сообщения в память — на диск ничего не пишем.

    Фото и картинки-файлы отдаём целиком, у видео, кружков и гифок — превью-кадр:
    модели нужна картинка, а не ролик.
    """
    msg = await client.get_messages(entity, ids=msg_id)
    if msg is None:
        raise ValueError(f"Сообщения {msg_id} нет (удалено или id не из этого чата).")

    mime = (getattr(msg.file, "mime_type", None) or "") if msg.file else ""
    if msg.photo:
        kind, fmt, thumb = "photo", "jpeg", None
    elif msg.sticker and mime in IMAGE_FORMATS:
        kind, fmt, thumb = "sticker", IMAGE_FORMATS[mime], None
    elif msg.document and mime in IMAGE_FORMATS:
        kind, fmt, thumb = "image_file", IMAGE_FORMATS[mime], None
    elif msg.video or msg.video_note or msg.gif or msg.sticker:
        kind, fmt, thumb = "preview_frame", "jpeg", -1
    else:
        raise ValueError(f"В сообщении {msg_id} нет изображения (media: {_media_label(msg)}).")

    if thumb is None and msg.file and msg.file.size and msg.file.size > MAX_IMAGE_BYTES:
        raise ValueError(
            f"Картинка слишком большая: {msg.file.size // 1024} КБ, предел {MAX_IMAGE_BYTES // 1024} КБ."
        )
    data = await client.download_media(msg, file=bytes, thumb=thumb)
    if not data:
        raise ValueError(f"У сообщения {msg_id} нет превью, показать нечего.")
    return {"data": data, "format": fmt, "kind": kind, "caption": msg.message or "", "bytes": len(data)}


def _safe_name(name: str) -> str:
    keep = "".join(ch if ch.isalnum() or ch in " ._-()[]" else "_" for ch in name).strip(" .")
    return keep[:120] or "file"


async def download_file(client: TelegramClient, entity, msg_id: int, dest_dir: Path, max_bytes: int) -> dict:
    """Скачать вложение целиком — документ, видео, кружок, голосовое, аудио, фото."""
    msg = await client.get_messages(entity, ids=msg_id)
    if msg is None:
        raise ValueError(f"Сообщения {msg_id} нет (удалено или id не из этого чата).")
    if not msg.file:
        raise ValueError(f"В сообщении {msg_id} нет файла (media: {_media_label(msg)}).")

    size = msg.file.size or 0
    if size > max_bytes:
        raise ValueError(
            f"Файл {size // (1024 * 1024)} МБ больше предела {max_bytes // (1024 * 1024)} МБ "
            "(TG_MAX_DOWNLOAD_MB в .env)."
        )
    name = msg.file.name or f"{_media_label(msg) or 'file'}{msg.file.ext or ''}"
    dest_dir.mkdir(parents=True, exist_ok=True)
    target = dest_dir / f"{msg_id}-{_safe_name(name)}"
    cached = target.exists() and target.stat().st_size == size
    if not cached:
        tmp = target.with_name(target.name + ".part")
        await client.download_media(msg, file=str(tmp))
        tmp.replace(target)
    return {
        "path": str(target),
        "name": name,
        "mime_type": msg.file.mime_type,
        "size": target.stat().st_size,
        "duration": getattr(msg.file, "duration", None),
        "cached": cached,
    }


TRANSCRIBABLE = {"voice", "video_note"}


class TranscriptCache:
    """Расшифровки по ключу «чат:сообщение» в data/transcripts.json.

    Голосовое не меняется, поэтому готовый текст храним навсегда — повторное
    чтение истории не дёргает Telegram заново.
    """

    def __init__(self, path: Path):
        self.path = path

    def _load(self) -> dict:
        try:
            return json.loads(self.path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return {}

    def get(self, key: str) -> str | None:
        return self._load().get(key)

    def put(self, key: str, text: str) -> None:
        with FileLock(self.path):
            data = self._load()
            data[key] = text
            tmp = self.path.with_suffix(".tmp")
            tmp.write_text(json.dumps(data, ensure_ascii=False, indent=1), encoding="utf-8")
            tmp.replace(self.path)


STT_HINT = (
    "скачай голосовое через tg_download_file и расшифруй своим STT (whisper и т.п.)"
)


def transcribe_timeout(duration: float | None) -> float:
    """Сколько ждать финала: замер — ~0,1 с работы на секунду записи
    (3 мин → 19 с). Берём с тройным запасом, но не дольше 5 минут."""
    return min(300.0, 30.0 + 0.3 * (duration or 60))


async def transcribe(client: TelegramClient, entity, msg_id: int, timeout: float = 60.0) -> dict:
    """Встроенная расшифровка Telegram (кнопка «→A»; Premium — без лимита).

    Telegram отдаёт её частями: сначала pending=True с растущим текстом, и
    только апдейт с pending=False содержит всё. Промежуточный текст — это
    обрезок, выдавать его за расшифровку нельзя: поэтому возвращаем
    status="done" только по финальному ответу, а обрезок — отдельным полем.
    """
    peer = await client.get_input_entity(entity)
    request = functions.messages.TranscribeAudioRequest(peer=peer, msg_id=msg_id)
    result = await client(request)
    if not result.pending:
        return {"status": "done", "text": result.text}

    loop = asyncio.get_running_loop()
    done: asyncio.Future = loop.create_future()
    latest = {"text": result.text or ""}

    async def on_update(update) -> None:
        if not isinstance(update, types.UpdateTranscribedAudio) or update.msg_id != msg_id:
            return
        if len(update.text or "") >= len(latest["text"]):
            latest["text"] = update.text or ""
        if not update.pending and not done.done():
            done.set_result(update.text)

    client.add_event_handler(on_update, events.Raw)
    try:
        deadline = loop.time() + timeout
        while loop.time() < deadline:
            try:
                text = await asyncio.wait_for(asyncio.shield(done), min(5.0, max(0.1, deadline - loop.time())))
                return {"status": "done", "text": text}
            except asyncio.TimeoutError:
                again = await client(request)
                if not again.pending:
                    return {"status": "done", "text": again.text}
                if len(again.text or "") >= len(latest["text"]):
                    latest["text"] = again.text or ""
        return {"status": "pending", "text": None, "partial": latest["text"]}
    finally:
        client.remove_event_handler(on_update, events.Raw)


def _apply_transcript(msg: dict, got: dict) -> None:
    """Разложить результат по полям так, чтобы обрезок не выглядел готовым текстом."""
    msg["transcript_status"] = got["status"]
    msg["transcript"] = got.get("text")
    if got["status"] == "pending":
        msg["transcript_partial"] = got.get("partial") or ""
        msg["transcript_hint"] = (
            "расшифровка ещё не закончена, transcript_partial — только начало. "
            f"Повтори tg_transcribe чуть позже (она ждёт дольше) или {STT_HINT}"
        )


async def attach_transcripts(
    client: TelegramClient,
    entity,
    rule: ChatRule,
    messages: list[dict],
    cache: TranscriptCache,
    budget: int,
    time_budget: float = 20.0,
) -> None:
    """Дописать расшифровку к голосовым и кружкам.

    Чтение чата не должно висеть минутами, поэтому здесь ждём в пределах
    общего бюджета времени; недождавшиеся голосовые помечаются pending, и
    агент доводит их через tg_transcribe, которая ждёт столько, сколько нужно.
    """
    loop = asyncio.get_running_loop()
    deadline = loop.time() + time_budget
    for msg in messages:
        if msg.get("media") not in TRANSCRIBABLE:
            continue
        key = f"{rule.alias}:{msg['id']}"
        cached = cache.get(key)
        if cached is not None:
            _apply_transcript(msg, {"status": "done", "text": cached})
            continue
        left = deadline - loop.time()
        if budget <= 0 or left < 5:
            msg["transcript"] = None
            msg["transcript_status"] = "not_requested"
            msg["transcript_hint"] = "не успел расшифровать в этом вызове — вызови tg_transcribe"
            continue
        budget -= 1
        try:
            got = await transcribe(
                client, entity, msg["id"], timeout=min(left, transcribe_timeout(msg.get("duration")))
            )
        except errors.RPCError as exc:
            msg["transcript"] = None
            msg["transcript_status"] = "error"
            msg["transcript_error"] = f"{type(exc).__name__}: {exc}"
            msg["transcript_hint"] = f"Telegram не расшифровал — {STT_HINT}"
            continue
        _apply_transcript(msg, got)
        if got["status"] == "done":
            cache.put(key, got["text"])


async def read_messages(
    client: TelegramClient,
    rule: ChatRule,
    *,
    limit: int = 50,
    before_id: int | None = None,
    after_id: int | None = None,
    search: str | None = None,
    cache: TranscriptCache | None = None,
    transcribe_budget: int = 0,
) -> list[dict]:
    entity = await entity_for(client, rule)
    kwargs: dict[str, Any] = {"limit": limit}
    if before_id:
        kwargs["max_id"] = before_id
    if after_id:
        kwargs["min_id"] = after_id
    if search:
        kwargs["search"] = search
    messages = [await _serialize(m) async for m in client.iter_messages(entity, **kwargs)]
    messages.reverse()  # хронологический порядок: старые сверху
    if cache is not None:
        await attach_transcripts(client, entity, rule, messages, cache, transcribe_budget)
    return messages


async def chat_status(client: TelegramClient, rules: list[ChatRule]) -> dict[int | str, dict]:
    """Непрочитанные и последнее сообщение — одним запросом на все чаты."""
    wanted: dict[str, ChatRule] = {}
    for rule in rules:
        entity = await entity_for(client, rule)
        ident = "me" if entity == "me" else str(getattr(entity, "id", entity))
        wanted[ident] = rule
    status: dict[str, dict] = {}
    me = await client.get_me()
    async for dialog in client.iter_dialogs(limit=None):
        ident = str(dialog.entity.id)
        if dialog.entity.id == me.id:
            ident = "me"
        if ident in wanted:
            status[wanted[ident].alias] = {
                "unread": dialog.unread_count,
                "last_message_at": dialog.date.isoformat() if dialog.date else None,
                "last_message_preview": (dialog.message.message or "")[:160] if dialog.message else "",
            }
    return status


async def send_files(
    client: TelegramClient, rule: ChatRule, paths: list[str], *, reply_to: int | None = None
) -> list[int]:
    """Отправить файлы как документы — без пережатия, одним альбомом до 10 штук."""
    entity = await entity_for(client, rule)
    sent = await client.send_file(entity, paths if len(paths) > 1 else paths[0], force_document=True, reply_to=reply_to)
    items = sent if isinstance(sent, list) else [sent]
    return [m.id for m in items]


async def send_message(
    client: TelegramClient,
    rule: ChatRule,
    text: str,
    *,
    reply_to: int | None = None,
    fmt: str = "markdown",
) -> dict:
    """Отправить от аккаунта владельца.

    markdown уходит как rich message (июнь 2026): заголовки, списки, таблицы,
    цитаты, разделители Telegram отрисовывает сам. Если сервер такое не примет,
    откатываемся на обычное сообщение с классической markdown-разметкой —
    сообщение важнее красоты.
    """
    entity = await entity_for(client, rule)
    if fmt == "markdown":
        peer = await client.get_input_entity(entity)
        request = functions.messages.SendMessageRequest(
            peer=peer,
            message="",
            rich_message=types.InputRichMessageMarkdown(markdown=text),
            reply_to=types.InputReplyToMessage(reply_to_msg_id=reply_to) if reply_to else None,
            no_webpage=True,
        )
        try:
            result = await client(request)
            sent = client._get_response_message(request, result, peer)
            return {
                "message_id": sent.id,
                "date": sent.date.isoformat() if getattr(sent, "date", None) else None,
                "format": "rich_markdown",
            }
        except errors.RPCError as exc:
            fallback_reason = f"{type(exc).__name__}: {exc}"
        sent = await client.send_message(entity, text, reply_to=reply_to, parse_mode="md", link_preview=False)
        return {
            "message_id": sent.id,
            "date": sent.date.isoformat() if sent.date else None,
            "format": "markdown_fallback",
            "fallback_reason": fallback_reason,
        }

    sent = await client.send_message(entity, text, reply_to=reply_to, parse_mode=None, link_preview=False)
    return {"message_id": sent.id, "date": sent.date.isoformat() if sent.date else None, "format": "plain"}


async def list_dialogs(client: TelegramClient, limit: int = 100) -> list[dict]:
    """Все диалоги аккаунта — только для человека (команда `tg dialogs`),
    агенту этот список не отдаётся."""
    rows = []
    async for dialog in client.iter_dialogs(limit=limit):
        entity = dialog.entity
        kind = (
            "user"
            if isinstance(entity, types.User)
            else "channel"
            if isinstance(entity, types.Channel) and entity.broadcast
            else "group"
        )
        rows.append(
            {
                "id": dialog.id,
                "title": dialog.name or "",
                "username": getattr(entity, "username", None),
                "kind": kind,
                "unread": dialog.unread_count,
                "archived": bool(getattr(dialog.dialog, "folder_id", None)),
            }
        )
    return rows
