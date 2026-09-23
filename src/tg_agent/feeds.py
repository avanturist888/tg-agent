"""Ленты новых сообщений: data/feeds/<alias>.ids — по id на строку.

Пишет только слушатель (демон): раз в несколько секунд одним запросом
смотрит верхние сообщения всех разрешённых чатов и дописывает новые id.
Агенты файл только читают — это не стоит ни сессии, ни сети, ни лока, и
его можно слушать через tail -f (подписка через Monitor в Claude Code).
"""

from __future__ import annotations

import asyncio
import time
from pathlib import Path

from telethon.tl import functions, types

from . import audit, client as tg
from .config import Settings
from .lock import SessionBusy


def feed_path(settings: Settings, alias: str) -> Path:
    return settings.data_dir / "feeds" / f"{alias}.ids"


def last_id(path: Path) -> int:
    """Последний id в ленте (0, если ленты ещё нет)."""
    try:
        with path.open("rb") as fh:
            fh.seek(0, 2)
            size = fh.tell()
            fh.seek(max(0, size - 64))
            tail = fh.read().decode("ascii", errors="ignore").split()
        return int(tail[-1]) if tail else 0
    except (OSError, ValueError):
        return 0


def _append(path: Path, ids: list[int]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="ascii", newline="\n") as fh:
        fh.writelines(f"{i}\n" for i in ids)


async def poll_once(settings: Settings) -> dict[str, int]:
    """Один проход: дописать новые id во все ленты. Возвращает, сколько добавлено."""
    rules = [r for r in settings.chats.values() if r.read]
    if not rules:
        return {}
    added: dict[str, int] = {}
    # короткое ожидание: сессия занята — пропускаем проход, а не стоим
    async with tg.open_client(settings, wait=0.5) as client:
        entities = {r.alias: await tg.entity_for(client, r) for r in rules}
        peers = {
            alias: types.InputDialogPeer(peer=await client.get_input_entity(e))
            for alias, e in entities.items()
        }
        res = await client(functions.messages.GetPeerDialogsRequest(peers=list(peers.values())))
        me = await client.get_me(input_peer=False)

        top: dict[str, int] = {}
        for dialog in res.dialogs:
            peer = dialog.peer
            pid = getattr(peer, "user_id", None) or getattr(peer, "chat_id", None) or getattr(peer, "channel_id", None)
            for alias, e in entities.items():
                eid = me.id if e == "me" else getattr(e, "id", None)
                if eid == pid:
                    top[alias] = dialog.top_message

        for alias, top_id in top.items():
            path = feed_path(settings, alias)
            known = last_id(path)
            if known == 0:
                # новая лента начинается с текущего момента — историю не льём
                _append(path, [top_id])
                continue
            if top_id <= known:
                continue
            new_ids = [
                m.id async for m in client.iter_messages(entities[alias], min_id=known, reverse=True)
                if m.id > known
            ]
            if new_ids:
                _append(path, new_ids)
                added[alias] = len(new_ids)
    return added


async def run_poller(settings_loader, interval: float) -> None:
    """Фоновый цикл внутри демона. Сбой одного прохода не роняет демона."""
    while True:
        try:
            settings = settings_loader()  # белый список мог поменяться
            await poll_once(settings)
        except SessionBusy:
            pass  # сессию держит отправка или чтение — зайдём на следующем круге
        except Exception as exc:  # noqa: BLE001
            try:
                audit.log(settings_loader().audit_path, "feed_poll_failed", error=f"{type(exc).__name__}: {exc}")
            except Exception:  # noqa: BLE001
                pass
        await asyncio.sleep(interval)


def watch(settings: Settings, aliases: list[str], every: float = 1.0) -> None:
    """Печатать «alias id» на каждое новое сообщение — для Monitor в Claude Code.

    Читает только локальные файлы лент, к Telegram не ходит.
    """
    rules = [settings.resolve(a) for a in aliases] if aliases else [r for r in settings.chats.values() if r.read]
    seen = {r.alias: last_id(feed_path(settings, r.alias)) for r in rules}
    print(f"слежу за: {', '.join(seen)} (Ctrl+C — выход)", flush=True)
    while True:
        for alias in seen:
            path = feed_path(settings, alias)
            try:
                lines = path.read_text(encoding="ascii").split()
            except OSError:
                continue
            fresh = [int(x) for x in lines if x.isdigit() and int(x) > seen[alias]]
            for mid in fresh:
                print(f"{alias} {mid}", flush=True)
            if fresh:
                seen[alias] = max(fresh)
        time.sleep(every)
