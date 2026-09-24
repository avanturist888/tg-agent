"""Подтверждение отправки кнопкой в боте.

Карточка с текстом черновика уходит в бот уведомлений, под ней две кнопки.
Нажатие обрабатывает тот, кто в этот момент держит лок на getUpdates: либо
демон `tg approvals`, либо сам ожидающий агент. Второго потребителя апдейтов
Telegram не допускает, поэтому лок здесь ещё и признак «кто-то уже слушает».
"""

from __future__ import annotations

import asyncio
import json
import time
from datetime import datetime, timezone

from . import attachments, audit, bot
from .config import Settings
from .lock import FileLock, SessionBusy
from .outbox import APPROVED, CANCELLED, EXPIRED, PENDING, SENT, Outbox

FINAL = {SENT, CANCELLED, EXPIRED}
POLL_TIMEOUT = 25


def _read_offset(settings: Settings) -> int:
    try:
        return int(json.loads(settings.offset_path.read_text(encoding="utf-8"))["offset"])
    except (OSError, ValueError, KeyError):
        return 0


def _write_offset(settings: Settings, offset: int) -> None:
    settings.offset_path.write_text(json.dumps({"offset": offset}), encoding="utf-8")


async def notify(settings: Settings, draft, rule_title: str, note: str = "") -> int | None:
    """Показать черновик в боте. Возвращает id карточки или None, если бот молчит."""
    try:
        message_id = await bot.send_draft_card(settings, draft, rule_title, note)
    except bot.BotError as exc:
        audit.log(settings.audit_path, "card_failed", draft_id=draft.id, error=str(exc))
        return None
    Outbox(settings.outbox_path).attach_card(draft.id, message_id)
    audit.log(settings.audit_path, "card_sent", draft_id=draft.id, message_id=message_id)
    return message_id


def _title(settings: Settings, draft) -> str:
    rule = settings.chats.get(draft.chat)
    return rule.title if rule else draft.chat


async def _card(settings: Settings, card_id: int | None, draft, status_md: str) -> None:
    """Переписать карточку итогом (строка статуса — в markdown)."""
    if card_id:
        await bot.finish_card(settings, card_id, draft, _title(settings, draft), status_md, draft.note)


async def _deliver(settings: Settings, draft_id: str, by: str) -> str:
    """Отправить подтверждённый черновик и вернуть строку итога для карточки."""
    from . import service  # внутри функции: service сам зовёт этот модуль

    outbox = Outbox(settings.outbox_path)
    draft = outbox.get(draft_id)
    # Человек уже нажал «Отправить»: временные сбои (сессия занята, сеть)
    # переживаем сами, прежде чем сдаваться и звать агента.
    result: dict | None = None
    error = ""
    for attempt in (1, 2, 3):
        try:
            result = await service.deliver_approved(settings, draft_id, by=by)
            break
        except service.Denied as exc:  # запрет (чат закрыт, статус не тот) — повтор не поможет
            error = f"{type(exc).__name__}: {exc}"
            break
        except Exception as exc:  # noqa: BLE001 — сессия занята, сеть, файл лока: пробуем ещё
            error = f"{type(exc).__name__}: {exc}"
            audit.log(settings.audit_path, "send_failed", draft_id=draft_id, attempt=attempt, error=str(exc))
            if attempt < 3:
                await asyncio.sleep(15)
    if not (result and result.get("sent")):
        # сбой мог случиться уже после отправки (при записи статуса) —
        # тогда сообщение ушло, и «Не отправлено» было бы враньём
        try:
            done = outbox.get(draft_id)
        except Exception:  # noqa: BLE001
            done = None
        if done and done.status == SENT:
            result = {"sent": True, "message_id": done.message_id}
    if not (result and result.get("sent")):
        outbox.mark_send_error(draft_id, error or "неизвестная ошибка")
        return f"⚠️ **Не отправлено** — {bot.md_escape(error)}"
    when = datetime.now().strftime("%H:%M")
    title = bot.md_escape(_title(settings, draft))
    return f"✅ **Отправлено** в «{title}» в {when} (id {result['message_id']})"


async def handle_callback(settings: Settings, query: dict) -> None:
    data = str(query.get("data", ""))
    callback_id = query["id"]
    sender = query.get("from", {}).get("id")
    card_id = query.get("message", {}).get("message_id")

    if sender != settings.approval_chat_id:
        await bot.answer_callback(settings, callback_id, "Эта кнопка не для вас.")
        audit.log(settings.audit_path, "callback_rejected", sender=sender, data=data)
        return
    if not data.startswith("d:"):
        return

    _, draft_id, verdict = (data.split(":", 2) + ["", ""])[:3]
    outbox = Outbox(settings.outbox_path)
    try:
        draft = outbox.get(draft_id)
    except KeyError:
        await bot.answer_callback(settings, callback_id, "Черновик не найден.")
        return

    # Под rich-сообщением callback может прийти без message — тогда карточку
    # молча не правили, и кнопки висели после отправки. Id карточки и так
    # записан в черновике при её отправке.
    if card_id != draft.bot_message_id:
        audit.log(
            settings.audit_path, "callback_card_mismatch", draft_id=draft_id, from_callback=card_id,
            stored=draft.bot_message_id,
        )
    card_id = draft.bot_message_id or card_id

    if draft.status in FINAL or draft.status == APPROVED:
        await bot.answer_callback(settings, callback_id, f"Уже {draft.status}.")
        await _card(settings, card_id, draft, f"⏹ Уже обработано ({draft.status}).")
        return
    if draft.is_expired():
        await bot.answer_callback(settings, callback_id, "Черновик протух.")
        await _card(settings, card_id, draft, "⌛ **Протухло** — агент готовил это давно.")
        return

    if verdict == "ok":
        outbox.approve(draft_id, by=f"telegram_button:{sender}")
        audit.log(settings.audit_path, "approve", draft_id=draft_id, chat=draft.chat, by="telegram_button")
        await bot.answer_callback(settings, callback_id, "Отправляю…")
        # кнопки убираем сразу: отправка может занять секунды, а повторное
        # нажатие за это время сбивает с толку
        await _card(settings, card_id, draft, "⏳ **Отправляю…**")
        summary = await _deliver(settings, draft_id, by="telegram_button")
    else:
        outbox.cancel(draft_id, by=f"telegram_button:{sender}")
        attachments.drop(settings, draft_id)
        audit.log(settings.audit_path, "reject", draft_id=draft_id, chat=draft.chat, by="telegram_button")
        await bot.answer_callback(settings, callback_id, "Отклонено.")
        summary = "✋ **Отклонено** — сообщение не ушло."

    await _card(settings, card_id, draft, summary)


async def close_card(settings: Settings, draft_id: str, result: dict | None = None, summary: str = "") -> None:
    """Погасить карточку, если черновик решили мимо кнопок — через CLI или отменой."""
    try:
        draft = Outbox(settings.outbox_path).get(draft_id)
    except KeyError:
        return
    if not summary:
        when = datetime.now().strftime("%H:%M")
        summary = f"✅ **Отправлено** в {when} (id {(result or {}).get('message_id')})"
    await _card(settings, draft.bot_message_id, draft, summary)


async def pump(settings: Settings, *, until: float, stop_when=None) -> None:
    """Качать апдейты, пока не истечёт время или не сработает stop_when.

    Вызывается только тем, кто держит лок на getUpdates.
    """
    offset = _read_offset(settings)
    while time.monotonic() < until:
        if stop_when and stop_when():
            return
        window = max(1, min(POLL_TIMEOUT, int(until - time.monotonic())))
        try:
            updates = await bot.get_updates(settings, offset, timeout=window)
        except bot.BotError as exc:
            audit.log(settings.audit_path, "poll_failed", error=str(exc))
            await asyncio.sleep(5)
            continue
        for update in updates:
            offset = max(offset, int(update["update_id"]) + 1)
            _write_offset(settings, offset)
            if "callback_query" in update:
                try:
                    await handle_callback(settings, update["callback_query"])
                except Exception as exc:  # noqa: BLE001 — одно нажатие не должно ронять слушателя
                    audit.log(settings.audit_path, "callback_failed", error=f"{type(exc).__name__}: {exc}")


async def wait_for_decision(settings: Settings, draft_id: str, timeout_sec: int) -> dict:
    """Дождаться нажатия кнопки. Работает и с демоном, и без него."""
    outbox = Outbox(settings.outbox_path)
    deadline = time.monotonic() + timeout_sec

    def resolved() -> bool:
        try:
            d = outbox.get(draft_id)
        except KeyError:
            return True
        # одобрено, но отправка сорвалась — агенту надо знать сразу, а не ждать таймаута
        return d.status in FINAL or (d.status == APPROVED and bool(d.send_error))

    lock = FileLock(settings.updates_lock_path, timeout=0.5)
    try:
        with lock:
            # слушателя нет — качаем апдейты сами, пока ждём
            await pump(settings, until=deadline, stop_when=resolved)
    except SessionBusy:
        # апдейты разбирает демон, нам остаётся следить за статусом
        while time.monotonic() < deadline and not resolved():
            await asyncio.sleep(2)

    try:
        draft = outbox.get(draft_id)
    except KeyError:
        return {"draft_id": draft_id, "status": "not_found"}
    if draft.status == APPROVED and draft.send_error:
        return {
            "draft_id": draft_id,
            "chat": draft.chat,
            "status": "send_failed",
            "error": draft.send_error,
            "hint": (
                "Владелец НАЖАЛ «Отправить», но отправка сорвалась (см. error). Черновик "
                "одобрен — повтори отправку через tg_send_draft(draft_id), новое "
                "подтверждение не нужно. Если снова не выйдет, скажи владельцу."
            ),
        }
    if draft.status == APPROVED:
        return {
            "draft_id": draft_id,
            "chat": draft.chat,
            "status": "sending",
            "hint": "Владелец нажал «Отправить», отправка идёт — проверь через tg_list_drafts чуть позже.",
        }
    return {
        "draft_id": draft_id,
        "chat": draft.chat,
        "status": draft.status if draft.status in FINAL else "waiting",
        "message_id": draft.message_id,
        "hint": (
            "Отправлено." if draft.status == SENT
            else "Человек отклонил отправку — не переспрашивай и не пересоздавай черновик без новой просьбы."
            if draft.status == CANCELLED
            else "Черновик протух." if draft.status == EXPIRED
            else "Человек пока не нажал кнопку. Карточка в боте жива: сообщи об этом и займись другим."
        ),
    }


async def recover_interrupted(settings: Settings) -> None:
    """Дослать то, что человек одобрил кнопкой, но прошлый слушатель не успел
    отправить — его убили между нажатием и отправкой (рестарт, перезагрузка).

    Трогаем только одобренное кнопкой в режиме bot_approval: в остальных
    режимах статус approved законно ждёт, пока отправит агент или CLI.
    """
    if settings.send_policy != "bot_approval":
        return
    for draft in Outbox(settings.outbox_path).list(status=APPROVED):
        by_button = any(
            h.get("event") == "approved" and str(h.get("by", "")).startswith("telegram_button")
            for h in draft.history
        )
        if not by_button:
            continue
        audit.log(settings.audit_path, "recover_interrupted", draft_id=draft.id, chat=draft.chat)
        summary = await _deliver(settings, draft.id, by="telegram_button (дослано после перезапуска слушателя)")
        await _card(settings, draft.bot_message_id, draft, summary)


def _normal_priority() -> None:
    """Планировщик запускает задачи с пониженным приоритетом, и Windows
    морозила слушателя на десятки секунд при почти пустом процессоре —
    нажатие ждало, пока ему дадут поработать."""
    import os

    if os.name == "nt":
        import ctypes

        kernel32 = ctypes.windll.kernel32
        kernel32.SetPriorityClass(kernel32.GetCurrentProcess(), 0x20)  # NORMAL_PRIORITY_CLASS


async def run_daemon(settings: Settings) -> None:
    """Постоянный слушатель кнопок: нажатие срабатывает, даже если агент ушёл."""
    print(f"Слушаю кнопки бота, чат {settings.approval_chat_id}. Ctrl+C — выход.")
    _normal_priority()
    # Лок может держать ожидающий агент (пока демона не было, кнопки разбирал
    # он). Выходить нельзя — тогда после ухода агента не останется никого.
    # Ждём, пока агент дождётся своего решения и отпустит лок.
    lock = FileLock(settings.updates_lock_path, timeout=0.5)
    while True:
        try:
            lock.__enter__()
            break
        except SessionBusy:
            await asyncio.sleep(5)  # асинхронно: не держим цикл событий
    from . import feeds
    from .config import load_settings

    poller = asyncio.create_task(feeds.run_poller(load_settings, settings.feed_interval))
    try:
        audit.log(settings.audit_path, "daemon_start")
        await recover_interrupted(settings)
        while True:
            lock.touch()  # иначе лок сочтут протухшим и перехватят
            await pump(settings, until=time.monotonic() + POLL_TIMEOUT)
    finally:
        poller.cancel()
        lock.__exit__(None, None, None)


async def sweep_once(settings: Settings) -> int:
    """Разобрать накопившиеся нажатия и выйти — если демон не крутится."""
    handled_before = _read_offset(settings)
    with FileLock(settings.updates_lock_path, timeout=5):
        await pump(settings, until=time.monotonic() + 3)
    return _read_offset(settings) - handled_before


def pending_summary(settings: Settings) -> list[dict]:
    drafts = Outbox(settings.outbox_path).list(status=PENDING)
    return [
        {
            "id": d.id,
            "chat": d.chat,
            "created_at": d.created_at,
            "card": d.bot_message_id,
            "text": d.text,
        }
        for d in drafts
    ]


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")
