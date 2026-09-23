from __future__ import annotations

from datetime import datetime, timezone

from . import approvals, attachments, audit, bot, client as tg
from .config import ROOT, Settings, load_settings
from .outbox import APPROVED, PENDING, Outbox


class Denied(PermissionError):
    pass


def _clamp(limit: int, settings: Settings) -> int:
    return max(1, min(int(limit or 50), settings.max_limit))


def _readable(settings: Settings, chat: str):
    rule = settings.resolve(chat)
    if not rule.read:
        raise Denied(f"Чат '{rule.alias}' есть в списке, но чтение для него выключено (read = false).")
    return rule


def _sendable(settings: Settings, chat: str):
    rule = settings.resolve(chat)
    if settings.send_policy == "disabled":
        raise Denied("Отправка сообщений отключена глобально (TG_SEND_POLICY=disabled).")
    if not rule.send:
        raise Denied(f"В чат '{rule.alias}' писать запрещено (send = false в config/chats.toml).")
    return rule


async def list_chats(settings: Settings, *, with_status: bool = False) -> dict:
    from . import feeds

    chats = []
    for rule in settings.chats.values():
        item = rule.as_dict()
        if rule.read:
            path = feeds.feed_path(settings, rule.alias)
            item["feed"] = {"path": str(path), "last_id": feeds.last_id(path)}
        chats.append(item)
    if with_status:
        async with tg.open_client(settings) as client:
            status = await tg.chat_status(client, list(settings.chats.values()))
        for item in chats:
            item.update(status.get(item["alias"], {}))
    return {"send_policy": settings.send_policy, "chats": chats}


async def read_chat(
    settings: Settings,
    chat: str,
    *,
    limit: int = 50,
    before_id: int | None = None,
    after_id: int | None = None,
    search: str | None = None,
    transcribe: bool = True,
) -> dict:
    rule = _readable(settings, chat)
    limit = _clamp(limit, settings)
    async with tg.open_client(settings) as client:
        messages = await tg.read_messages(
            client,
            rule,
            limit=limit,
            before_id=before_id,
            after_id=after_id,
            search=search,
            cache=tg.TranscriptCache(settings.transcripts_path) if transcribe else None,
            transcribe_budget=settings.transcribe_per_call,
        )
    audit.log(
        settings.audit_path,
        "read",
        chat=rule.alias,
        limit=limit,
        search=search,
        returned=len(messages),
    )
    return {
        "chat": rule.alias,
        "title": rule.title,
        "count": len(messages),
        "oldest_id": messages[0]["id"] if messages else None,
        "newest_id": messages[-1]["id"] if messages else None,
        "messages": messages,
    }


async def download_file(settings: Settings, chat: str, message_id: int) -> dict:
    """Скачать вложение из разрешённого чата в data/downloads/<alias>/."""
    rule = _readable(settings, chat)
    async with tg.open_client(settings) as client:
        entity = await tg.entity_for(client, rule)
        got = await tg.download_file(
            client,
            entity,
            message_id,
            settings.downloads_dir / rule.alias,
            settings.max_download_mb * 1024 * 1024,
        )
    audit.log(settings.audit_path, "download", chat=rule.alias, message_id=message_id, name=got["name"], size=got["size"])
    return {"chat": rule.alias, "message_id": message_id, **got}


async def react(settings: Settings, chat: str, message_id: int, emoji: str | None) -> dict:
    """Реакция на сообщение — сразу, без карточки: эмодзи на чужое сообщение
    ничего не может унести из контекста, а кнопка на каждый 👍 была бы пыткой."""
    if not settings.reactions:
        raise Denied("Реакции выключены владельцем (TG_REACTIONS=off).")
    rule = _sendable(settings, chat)
    emoji = (emoji or "").strip() or None
    if emoji and len(emoji) > 8:
        raise ValueError("Реакция — это один эмодзи, например 👍.")
    async with tg.open_client(settings) as client:
        entity = await tg.entity_for(client, rule)
        try:
            reactions = await tg.set_reaction(client, entity, message_id, emoji)
        except tg.errors.RPCError as exc:
            hint = {
                "REACTION_INVALID": "в этом чате такая реакция недоступна — попробуй 👍 ❤ 🔥 👏 😁 🎉",
                "REACTION_EMPTY": "реакция не указана",
                "MESSAGE_ID_INVALID": "нет такого сообщения в этом чате",
                "MESSAGE_NOT_MODIFIED": "такая реакция уже стоит",
            }.get(getattr(exc, "message", ""), "")
            raise ValueError(f"Telegram отказал: {exc}{' — ' + hint if hint else ''}") from exc
    audit.log(settings.audit_path, "react", chat=rule.alias, message_id=message_id, emoji=emoji or "(снята)")
    return {"chat": rule.alias, "message_id": message_id, "emoji": emoji, "reactions_now": reactions}


async def view_media(settings: Settings, chat: str, message_id: int) -> tuple[dict, bytes, str]:
    """Изображение из разрешённого чата — для показа модели."""
    rule = _readable(settings, chat)
    async with tg.open_client(settings) as client:
        entity = await tg.entity_for(client, rule)
        got = await tg.fetch_image(client, entity, message_id)
    audit.log(settings.audit_path, "view_media", chat=rule.alias, message_id=message_id, kind=got["kind"])
    meta = {
        "chat": rule.alias,
        "message_id": message_id,
        "kind": got["kind"],
        "caption": got["caption"],
        "bytes": got["bytes"],
    }
    return meta, got["data"], got["format"]


async def transcribe_message(settings: Settings, chat: str, message_id: int) -> dict:
    """Расшифровать одно голосовое или кружок встроенной функцией Telegram.

    Ждёт ПОЛНЫЙ текст, но сессию между заходами отпускает: пока Telegram
    расшифровывает длинную запись, остальные (отправка, чтение) не стоят.
    Повторный запрос отдаёт текущее состояние той же расшифровки.
    """
    import asyncio
    import time

    rule = _readable(settings, chat)
    cache = tg.TranscriptCache(settings.transcripts_path)
    key = f"{rule.alias}:{message_id}"
    cached = cache.get(key)
    if cached is not None:
        return {"chat": rule.alias, "message_id": message_id, "transcript_status": "done",
                "transcript": cached, "cached": True}

    duration = None
    deadline = None
    got: dict = {"status": "pending", "text": None, "partial": ""}
    while True:
        async with tg.open_client(settings) as client:
            entity = await tg.entity_for(client, rule)
            if deadline is None:
                msg = await client.get_messages(entity, ids=message_id)
                if msg is None or not (msg.voice or msg.video_note):
                    raise ValueError(f"Сообщение {message_id} — не голосовое и не кружок.")
                duration = getattr(msg.file, "duration", None)
                deadline = time.monotonic() + tg.transcribe_timeout(duration)
            try:
                got = await tg.transcribe(client, entity, message_id, timeout=8)
            except tg.errors.RPCError as exc:
                audit.log(settings.audit_path, "transcribe", chat=rule.alias, message_id=message_id, error=str(exc))
                return {
                    "chat": rule.alias, "message_id": message_id, "duration": duration,
                    "transcript_status": "error", "transcript": None,
                    "transcript_error": f"{type(exc).__name__}: {exc}",
                    "transcript_hint": f"Telegram не расшифровал — {tg.STT_HINT}",
                }
        if got["status"] == "done" or time.monotonic() >= deadline:
            break
        await asyncio.sleep(4)  # сессия свободна — пусть поработают другие

    if got["status"] == "done":
        cache.put(key, got["text"])
    audit.log(settings.audit_path, "transcribe", chat=rule.alias, message_id=message_id, status=got["status"])
    out = {"chat": rule.alias, "message_id": message_id, "duration": duration, "cached": False}
    tg._apply_transcript(out, got)
    return out


async def draft_message(
    settings: Settings,
    chat: str,
    text: str,
    *,
    reply_to: int | None = None,
    note: str = "",
    fmt: str = "markdown",
    files: list[str] | None = None,
) -> dict:
    rule = _sendable(settings, chat)
    text = (text or "").strip()
    if not text and not files:
        raise ValueError("Пустое сообщение: нужен текст или файлы.")
    if fmt not in {"markdown", "plain"}:
        raise ValueError("format: только 'markdown' или 'plain'.")
    if len(text) > 32768:
        raise ValueError(f"Слишком длинно: {len(text)} символов, предел Telegram — 32768.")
    outbox = Outbox(settings.outbox_path)
    draft_id = outbox.new_id()
    snapshots = attachments.snapshot(settings, draft_id, files) if files else []
    draft = outbox.create(
        chat=rule.alias,
        text=text,
        reply_to=reply_to,
        ttl_min=settings.draft_ttl_min,
        note=note,
        fmt=fmt,
        files=snapshots,
        draft_id=draft_id,
    )
    audit.log(
        settings.audit_path,
        "draft",
        draft_id=draft.id,
        chat=rule.alias,
        chars=len(text),
        files=[{"name": f["name"], "size": f["size"], "source": f["source"]} for f in snapshots],
    )

    if settings.send_policy == "bot_approval":
        card = await approvals.notify(settings, draft, rule.title, note)
        instruction = (
            "Карточка с текстом ушла в бот подтверждений — человек нажмёт кнопку там. "
            "Вызови tg_wait_approval с этим draft_id и дождись результата; "
            "нажатие «Отправить» отправляет сообщение само."
            if card
            else "Бот подтверждений недоступен, карточку показать не удалось. "
            "Скажи человеку и предложи подтвердить командой: "
            f"! uv run --directory {ROOT} tg approve {draft.id} --send"
        )
        return {
            "draft_id": draft.id,
            "chat": rule.alias,
            "text": draft.text,
            "reply_to": draft.reply_to,
            "expires_at": draft.expires_at,
            "files": [{"name": f["name"], "size": f["size"]} for f in draft.files],
            "send_policy": settings.send_policy,
            "card_sent": bool(card),
            "next_step": instruction,
        }

    if settings.send_policy == "human_approval":
        instruction = (
            "Черновик создан, НО НЕ ОТПРАВЛЕН. Покажи пользователю текст целиком и попроси "
            f"подтвердить командой:  ! uv run --directory {ROOT} tg approve {draft.id}\n"
            "После подтверждения вызови tg_send_draft с этим draft_id."
        )
    else:
        instruction = (
            "Черновик создан, НО НЕ ОТПРАВЛЕН. Покажи пользователю текст целиком, дождись "
            "явного согласия в диалоге и только тогда вызови tg_send_draft "
            "(передай в user_confirmation дословную фразу пользователя)."
        )
    return {
        "draft_id": draft.id,
        "chat": rule.alias,
        "text": draft.text,
        "reply_to": draft.reply_to,
        "expires_at": draft.expires_at,
        "send_policy": settings.send_policy,
        "next_step": instruction,
    }


async def wait_approval(settings: Settings, draft_id: str, timeout_sec: int | None = None) -> dict:
    """Дождаться, пока человек нажмёт кнопку под карточкой в боте."""
    if settings.send_policy != "bot_approval":
        raise Denied(
            f"Режим подтверждения — {settings.send_policy}, кнопок в боте нет. "
            "Дождись, пока человек подтвердит черновик, и вызови tg_send_draft."
        )
    Outbox(settings.outbox_path).get(draft_id)  # KeyError, если такого нет
    limit = min(int(timeout_sec or settings.approval_timeout_sec), settings.approval_timeout_sec)
    return await approvals.wait_for_decision(settings, draft_id, limit)


async def deliver_approved(settings: Settings, draft_id: str, *, by: str) -> dict:
    """Отправить черновик, который уже получил статус approved.

    Политику здесь не проверяем — она проверена там, где статус выставлялся
    (кнопка в боте или `tg approve`). Единственная точка, где сообщение
    реально уходит в Telegram.
    """
    outbox = Outbox(settings.outbox_path)
    draft = outbox.get(draft_id)
    rule = _sendable(settings, draft.chat)
    if draft.status != APPROVED:
        raise Denied(f"Черновик {draft_id} в статусе '{draft.status}', отправить нельзя.")
    result: dict = {}
    # человек уже нажал «Отправить» — ждём сессию сколько нужно, а не падаем
    async with tg.open_client(settings, wait=300) as client:
        if draft.text:
            result = await tg.send_message(client, rule, draft.text, reply_to=draft.reply_to, fmt=draft.fmt)
        if draft.files:
            # без текста файлы отвечают на reply_to сами, с текстом — идут следом
            file_ids = await tg.send_files(
                client,
                rule,
                [f["path"] for f in draft.files],
                reply_to=None if draft.text else draft.reply_to,
            )
            result.setdefault("message_id", file_ids[0])
            result["file_message_ids"] = file_ids
    outbox.mark_sent(draft_id, result["message_id"], datetime.now(timezone.utc).isoformat(timespec="seconds"))
    attachments.drop(settings, draft_id)
    audit.log(
        settings.audit_path,
        "send",
        draft_id=draft_id,
        chat=rule.alias,
        message_id=result["message_id"],
        format=result.get("format"),
        fallback_reason=result.get("fallback_reason"),
        approved_by=by,
        text=draft.text,
        files=[f["name"] for f in draft.files],
    )
    return {"draft_id": draft_id, "chat": rule.alias, "sent": True, **result}


async def send_draft(settings: Settings, draft_id: str, *, user_confirmation: str = "") -> dict:
    outbox = Outbox(settings.outbox_path)
    draft = outbox.get(draft_id)
    _sendable(settings, draft.chat)

    if draft.status == PENDING and settings.send_policy == "bot_approval":
        raise Denied(
            f"Черновик {draft_id} ждёт кнопки в боте — человек ещё не нажал «Отправить». "
            "Вызови tg_wait_approval вместо этого."
        )
    if draft.status == PENDING and settings.send_policy == "human_approval":
        raise Denied(
            f"Черновик {draft_id} не подтверждён человеком. Нужна команда:\n"
            f"  ! uv run --directory {ROOT} tg approve {draft_id}"
        )
    if draft.status == PENDING and settings.send_policy == "agent_confirm":
        if len(user_confirmation.strip()) < 2:
            raise Denied(
                "Нужно согласие пользователя: передай в user_confirmation его дословный ответ. "
                "Выдумывать подтверждение нельзя."
            )
        outbox.approve(draft_id, by=f"agent_confirm: {user_confirmation.strip()[:200]}")

    result = await deliver_approved(settings, draft_id, by=user_confirmation.strip()[:200] or "owner")
    if draft.bot_message_id:
        await approvals.close_card(settings, draft_id, result)
    return result


async def list_drafts(settings: Settings, status: str | None = None) -> dict:
    drafts = Outbox(settings.outbox_path).list(status=status)
    return {
        "send_policy": settings.send_policy,
        "drafts": [
            {
                "id": d.id,
                "chat": d.chat,
                "status": d.status,
                "text": d.text,
                "created_at": d.created_at,
                "expires_at": d.expires_at,
                "message_id": d.message_id,
                **({"send_error": d.send_error} if d.send_error else {}),
            }
            for d in drafts
        ],
    }


async def cancel_draft(settings: Settings, draft_id: str, by: str = "agent") -> dict:
    draft = Outbox(settings.outbox_path).cancel(draft_id, by=by)
    attachments.drop(settings, draft_id)
    audit.log(settings.audit_path, "cancel", draft_id=draft_id, chat=draft.chat, by=by)
    if draft.bot_message_id:
        await approvals.close_card(settings, draft_id, summary=f"✋ **Отменено** ({bot.md_escape(by)}).")
    return {"draft_id": draft_id, "status": draft.status}


def settings_or_die() -> Settings:
    return load_settings()
