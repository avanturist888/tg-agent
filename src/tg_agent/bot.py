"""Клиент Bot API: карточки подтверждения и кнопки под ними.

Отдельный канал от пользовательской сессии Telethon: черновик показывает бот
уведомлений, а отправляет уже аккаунт владельца.
"""

from __future__ import annotations

import asyncio
import html
import pathlib
from typing import Any

import httpx

from . import audit
from .config import Settings

API = "https://api.telegram.org"
TIMEOUT = httpx.Timeout(15.0, read=60.0)


class BotError(RuntimeError):
    pass


async def call(settings: Settings, method: str, payload: dict | None = None) -> Any:
    """Вызов Bot API: сначала напрямую, при сетевом отказе — через прокси.

    Провайдер может резать api.telegram.org (так уже было), а туннель умеет
    молча протухать. Поэтому пробуем оба маршрута, как это делает notify.mjs.
    """
    if not settings.bot_token:
        raise BotError("Не задан токен бота подтверждений.")
    url = f"{API}/bot{settings.bot_token}/{method}"
    routes: list[str | None] = [None]
    if settings.bot_proxy:
        routes.append(settings.bot_proxy)

    last: Exception | None = None
    for proxy in routes:
        for attempt in (1, 2):  # канал до Telegram джиттерит, одна осечка — не отказ
            try:
                async with httpx.AsyncClient(timeout=TIMEOUT, proxy=proxy) as http:
                    response = await http.post(url, json=payload or {})
                data = response.json()
                if not data.get("ok"):
                    raise BotError(f"{method}: {data.get('description', response.text)}")
                return data["result"]
            except (httpx.TransportError, httpx.TimeoutException) as exc:
                last = exc
                if attempt == 1:
                    await asyncio.sleep(1.5)
    raise BotError(f"{method}: сеть недоступна ни напрямую, ни через прокси ({last})")


BOT_UPLOAD_LIMIT = 50 * 1024 * 1024  # Bot API не принимает файлы больше


async def upload_document(settings: Settings, path: str, caption: str) -> int:
    """sendDocument с файлом из снимка черновика (multipart, не JSON)."""
    if not settings.bot_token:
        raise BotError("Не задан токен бота подтверждений.")
    url = f"{API}/bot{settings.bot_token}/sendDocument"
    routes: list[str | None] = [None] + ([settings.bot_proxy] if settings.bot_proxy else [])
    timeout = httpx.Timeout(30.0, write=300.0, read=120.0)
    last: Exception | None = None
    for proxy in routes:
        try:
            with open(path, "rb") as fh:
                async with httpx.AsyncClient(timeout=timeout, proxy=proxy) as http:
                    response = await http.post(
                        url,
                        data={
                            "chat_id": str(settings.approval_chat_id),
                            "caption": caption[:1000],
                            "disable_content_type_detection": "true",
                        },
                        files={"document": (pathlib.Path(path).name, fh)},
                    )
            data = response.json()
            if not data.get("ok"):
                raise BotError(f"sendDocument: {data.get('description', response.text)}")
            return int(data["result"]["message_id"])
        except (httpx.TransportError, httpx.TimeoutException) as exc:
            last = exc
    raise BotError(f"sendDocument: сеть недоступна ({last})")


def _file_label(f: dict, *, md: bool) -> str:
    from .attachments import human_size

    name = md_escape(f["name"]) if md else html.escape(f["name"])
    extra = "" if f["size"] <= BOT_UPLOAD_LIMIT else " — не показан, больше 50 МБ"
    return f"{name} ({human_size(f['size'])}{extra})"


MD_SPECIAL = set("\\`*_[]()#+-.!|>~=<{}$^")


def md_escape(text: str) -> str:
    """Экранировать служебные символы rich markdown в подставляемых значениях."""
    return "".join("\\" + ch if ch in MD_SPECIAL else ch for ch in text)


def html_from_status(status_md: str) -> str:
    """Строка статуса в markdown (**жирный**) → HTML для запасной карточки."""
    parts = status_md.split("**")
    return "".join(html.escape(p) if i % 2 == 0 else f"<b>{html.escape(p)}</b>" for i, p in enumerate(parts))


# Видимая черта текстом: rich-блок Divider (---) Telegram рисует почти незаметно
SEPARATOR = "─" * 24


def card_markdown(draft, title: str, note: str = "", status_md: str = "") -> str:
    """Карточка: шапка, разделитель, сам текст — отрисованный так, как уйдёт."""
    head = [
        "🤖 **Агент просит отправить сообщение**",
        f"Чат: **{md_escape(title)}** (`{draft.chat}`)",
    ]
    if note:
        head.append(f"Повод: {md_escape(note)}")
    if draft.reply_to:
        head.append(f"Ответом на сообщение #{draft.reply_to}")
    if getattr(draft, "files", None):
        head.append("📎 Файлы: " + ", ".join(_file_label(f, md=True) for f in draft.files))
    if not draft.text:
        body = "*без текста — только файлы*"
    else:
        body = draft.text if getattr(draft, "fmt", "markdown") == "markdown" else md_escape(draft.text)
    card = "\n\n".join(head) + f"\n\n{SEPARATOR}\n\n" + body
    if status_md:
        card += f"\n\n{SEPARATOR}\n\n" + status_md
    return card


def card_html(draft, title: str, note: str = "", status_md: str = "") -> str:
    """Запасная карточка на старом HTML — если rich markdown не прошёл."""
    head = [
        "🤖 <b>Агент просит отправить сообщение</b>",
        f"Чат: <b>{html.escape(title)}</b> (<code>{html.escape(draft.chat)}</code>)",
    ]
    if note:
        head.append(f"Повод: {html.escape(note)}")
    if draft.reply_to:
        head.append(f"Ответом на сообщение #{draft.reply_to}")
    if getattr(draft, "files", None):
        head.append("📎 Файлы: " + ", ".join(_file_label(f, md=False) for f in draft.files))
    card = "\n".join(head) + f"\n\n<pre>{html.escape(draft.text)}</pre>"
    if status_md:
        card += f"\n\n{html_from_status(status_md)}"
    return card


async def refresh_card(settings: Settings, message_id: int, draft, title: str, note: str = "") -> None:
    """Перерисовать ещё не решённую карточку, сохранив кнопки."""
    await call(
        settings,
        "editMessageText",
        {
            "chat_id": settings.approval_chat_id,
            "message_id": message_id,
            "rich_message": {"markdown": card_markdown(draft, title, note)},
            "reply_markup": keyboard(draft.id),
        },
    )


def keyboard(draft_id: str) -> dict:
    return {
        "inline_keyboard": [
            [
                {"text": "✅ Отправить", "callback_data": f"d:{draft_id}:ok"},
                {"text": "✋ Отклонить", "callback_data": f"d:{draft_id}:no"},
            ]
        ]
    }


async def send_draft_card(settings: Settings, draft, rule_title: str, note: str = "") -> int:
    # Сначала сами файлы — чтобы было видно, что именно уйдёт, — потом карточка с кнопками
    for f in getattr(draft, "files", None) or []:
        if f["size"] > BOT_UPLOAD_LIMIT:
            continue
        try:
            await upload_document(settings, f["path"], f"📎 {f['name']} — к черновику {draft.id}")
        except BotError as exc:
            audit.log(settings.audit_path, "card_file_failed", draft_id=draft.id, name=f["name"], error=str(exc))
    base = {"chat_id": settings.approval_chat_id, "reply_markup": keyboard(draft.id)}
    try:
        result = await call(
            settings,
            "sendRichMessage",
            {**base, "rich_message": {"markdown": card_markdown(draft, rule_title, note)}},
        )
    except BotError as exc:
        # markdown агента мог не пройти разбор — карточку всё равно показываем
        audit.log(settings.audit_path, "card_rich_failed", draft_id=draft.id, error=str(exc))
        result = await call(
            settings,
            "sendMessage",
            {
                **base,
                "text": card_html(draft, rule_title, note),
                "parse_mode": "HTML",
                "disable_web_page_preview": True,
            },
        )
    return int(result["message_id"])


async def finish_card(
    settings: Settings, message_id: int, draft, title: str, status_md: str, note: str = ""
) -> None:
    """Переписать карточку итогом и снять кнопки, чтобы не нажали дважды.

    Вызов без reply_markup снимает клавиатуру. Каждая неудача идёт в журнал:
    молчаливый проглот однажды уже оставил кнопки висеть под обработанным
    черновиком.
    """
    base = {"chat_id": settings.approval_chat_id, "message_id": message_id}
    attempts = [
        ("rich", {**base, "rich_message": {"markdown": card_markdown(draft, title, note, status_md)}}),
        ("html", {**base, "text": card_html(draft, title, note, status_md)[:4000],
                  "parse_mode": "HTML", "disable_web_page_preview": True}),
    ]
    for kind, payload in attempts:
        try:
            await call(settings, "editMessageText", payload)
            return
        except BotError as exc:
            audit.log(settings.audit_path, "card_edit_failed", message_id=message_id, kind=kind, error=str(exc))

    try:
        await call(settings, "editMessageReplyMarkup", base)
    except BotError as exc:
        audit.log(settings.audit_path, "card_strip_failed", message_id=message_id, error=str(exc))


async def answer_callback(settings: Settings, callback_id: str, text: str) -> None:
    try:
        await call(
            settings,
            "answerCallbackQuery",
            {"callback_query_id": callback_id, "text": text[:190]},
        )
    except BotError:
        pass  # всплывашка не критична, главное — сама отправка


async def get_updates(settings: Settings, offset: int, timeout: int = 25) -> list[dict]:
    return await call(
        settings,
        "getUpdates",
        {"offset": offset, "timeout": timeout, "allowed_updates": ["callback_query"]},
    )
