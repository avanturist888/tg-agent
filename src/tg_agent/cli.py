"""Командная строка для владельца аккаунта. Агенты сюда не ходят — у них MCP."""

from __future__ import annotations

import argparse
import asyncio
import json
import logging
import sys

from telethon import TelegramClient

from . import approvals, audit, bot, client as tg, service
from .config import ROOT, ConfigError, load_chats, load_settings
from .lock import FileLock, SessionBusy
from .outbox import PENDING, Outbox


def _print(data: object) -> None:
    print(json.dumps(data, ensure_ascii=False, indent=2))


async def cmd_login(args) -> int:
    settings = load_settings()
    phone = args.phone or settings.phone
    if not phone:
        phone = input("Телефон (+7...): ").strip()
    with FileLock(settings.session_path):
        client = TelegramClient(
            str(settings.session_path),
            settings.api_id,
            settings.api_hash,
            proxy=settings.proxy,
            device_model="tg-agent",
            system_version="claude-code",
            app_version="0.1.0",
        )
        await client.start(phone=lambda: phone)
        me = await client.get_me()
        await client.disconnect()
    audit.log(settings.audit_path, "login", user_id=me.id, username=me.username)
    print(f"Готово. Сессия сохранена: {settings.session_path}")
    print(f"Аккаунт: {me.first_name or ''} @{me.username or '—'} (id {me.id})")
    return 0


async def cmd_logout(args) -> int:
    settings = load_settings()
    async with tg.open_client(settings, require_auth=False) as client:
        await client.log_out()
    audit.log(settings.audit_path, "logout")
    print("Сессия отозвана на стороне Telegram.")
    return 0


async def cmd_whoami(args) -> int:
    settings = load_settings()
    async with tg.open_client(settings) as client:
        me = await client.get_me()
    _print(
        {
            "id": me.id,
            "username": me.username,
            "name": me.first_name,
            "session": str(settings.session_path),
            "send_policy": settings.send_policy,
            "chats_allowed": len(settings.chats),
        }
    )
    return 0


async def cmd_dialogs(args) -> int:
    """Все диалоги аккаунта — чтобы выбрать, что вписать в белый список.

    Публичная ссылка не нужна: id берётся из твоей же сессии. Архивные чаты
    Telethon отдаёт наравне с обычными.
    """
    settings = load_settings()
    async with tg.open_client(settings) as client:
        rows = await tg.list_dialogs(client, limit=args.limit)
    needle = (args.filter or "").lower()
    shown = 0
    for row in rows:
        if needle and needle not in (row["title"] or "").lower():
            continue
        uname = f" @{row['username']}" if row["username"] else ""
        flag = " [архив]" if row["archived"] else ""
        print(f"{row['id']:>16}  {row['kind']:<7} {row['title']}{uname}{flag}")
        shown += 1
    if not shown:
        print("Ничего не нашлось. Попробуй другой --filter или увеличь --limit.", file=sys.stderr)
        return 1
    print(f"\nНайдено: {shown}. Добавить в белый список: tg allow <alias> --id <id>", file=sys.stderr)
    return 0


def _update_rights(args) -> int:
    """`tg allow <существующий alias> [--send] [--no-read]` — переписать права.

    Без --send отправка выключается, как и при добавлении: права задаются
    целиком, а не накапливаются.
    """
    path = ROOT / "config" / "chats.toml"
    lines = path.read_text(encoding="utf-8").splitlines(keepends=True)
    inside = False
    for i, line in enumerate(lines):
        stripped = line.strip()
        if stripped == "[[chat]]":
            inside = False
        elif stripped.replace(" ", "") == f'alias="{args.alias}"':
            inside = True
        elif inside and stripped.startswith("read"):
            lines[i] = f"read = {'false' if args.no_read else 'true'}\n"
        elif inside and stripped.startswith("send"):
            lines[i] = f"send = {'true' if args.send else 'false'}\n"
    path.write_text("".join(lines), encoding="utf-8")
    rule = load_chats(path)[args.alias]
    print(f"Права '{args.alias}' обновлены: read = {str(rule.read).lower()}, send = {str(rule.send).lower()}")
    return 0


async def cmd_allow(args) -> int:
    """Дописать чат в config/chats.toml, не редактируя файл руками."""
    settings = load_settings()
    if args.alias.lower() in {a.lower() for a in settings.chats}:
        if args.id or args.match:
            print(f"Алиас '{args.alias}' уже занят другим чатом.", file=sys.stderr)
            return 1
        return _update_rights(args)

    peer, title = args.id, args.title
    if args.match:
        async with tg.open_client(settings) as client:
            rows = await tg.list_dialogs(client, limit=args.limit)
        needle = args.match.lower()
        found = [r for r in rows if needle in (r["title"] or "").lower()]
        if not found:
            print(f"По '{args.match}' ничего не нашлось.", file=sys.stderr)
            return 1
        if len(found) > 1:
            print("Под запрос подходит несколько чатов — уточни --match или задай --id:", file=sys.stderr)
            for r in found[:10]:
                print(f"  {r['id']:>16}  {r['kind']:<7} {r['title']}")
            return 1
        peer, title = found[0]["id"], title or found[0]["title"]
    if peer is None:
        print("Нужен --id или --match.", file=sys.stderr)
        return 1

    value = f'"{peer}"' if isinstance(peer, str) and not str(peer).lstrip("-").isdigit() else str(peer)
    block = (
        f'\n[[chat]]\nalias = "{args.alias}"\nid = {value}\n'
        f'title = "{(title or args.alias).replace(chr(34), chr(39))}"\n'
        f"read = {'true' if not args.no_read else 'false'}\n"
        f"send = {'true' if args.send else 'false'}\n"
    )
    path = ROOT / "config" / "chats.toml"
    with path.open("a", encoding="utf-8") as fh:
        fh.write(block)

    load_chats(path)  # сразу проверяем, что файл не сломался
    print(f"Добавлено в белый список:{block}")
    print("Перезапускать ничего не нужно — сервер перечитает файл на следующем вызове.")
    return 0


EXAMPLE_API_ID = 1234567


async def cmd_chats(args) -> int:
    settings = load_settings()
    _print(await service.list_chats(settings, with_status=args.status))
    return 0


async def cmd_read(args) -> int:
    settings = load_settings()
    _print(await service.read_chat(settings, args.chat, limit=args.limit, search=args.search))
    return 0


async def cmd_drafts(args) -> int:
    settings = load_settings()
    data = await service.list_drafts(settings, args.status)
    if args.json:
        _print(data)
        return 0
    if not data["drafts"]:
        print("Черновиков нет.")
        return 0
    for d in data["drafts"]:
        print(f"[{d['status']}] {d['id']} → {d['chat']}  (истекает {d['expires_at']})")
        for line in d["text"].splitlines() or [""]:
            print(f"    {line}")
        print()
    return 0


async def cmd_approve(args) -> int:
    settings = load_settings()
    outbox = Outbox(settings.outbox_path)
    draft = outbox.get(args.draft_id)
    print(f"Чат: {draft.chat}\n--- текст ---\n{draft.text}\n-------------")
    if draft.status != PENDING:
        print(f"Статус: {draft.status} — подтверждать нечего.")
        return 1
    outbox.approve(args.draft_id)
    audit.log(settings.audit_path, "approve", draft_id=args.draft_id, chat=draft.chat, by="human")
    print(f"Подтверждено: {args.draft_id}")
    if args.send:
        _print(await service.send_draft(settings, args.draft_id, user_confirmation="tg approve --send"))
    else:
        print("Агент может отправить его через tg_send_draft (или запусти `tg send <id>`).")
    return 0


async def cmd_reject(args) -> int:
    settings = load_settings()
    _print(await service.cancel_draft(settings, args.draft_id, by="human"))
    return 0


async def cmd_send(args) -> int:
    settings = load_settings()
    _print(await service.send_draft(settings, args.draft_id, user_confirmation="cli send by owner"))
    return 0


async def cmd_approvals(args) -> int:
    """Слушатель кнопок под карточками в боте."""
    settings = load_settings()
    if not settings.bot_ready:
        print("Бот подтверждений не настроен: нет токена или chat_id.", file=sys.stderr)
        return 1
    if args.once:
        handled = await approvals.sweep_once(settings)
        print(f"Разобрано накопившихся нажатий: {handled}")
        return 0
    try:
        await approvals.run_daemon(settings)
    except SessionBusy:
        print("Слушатель уже запущен другим процессом.", file=sys.stderr)
        return 1
    return 0


async def cmd_watch(args) -> int:
    """Подписка: строка «alias id» на каждое новое сообщение (для Monitor)."""
    from . import feeds

    try:
        feeds.watch(load_settings(), args.chats)
    except KeyboardInterrupt:
        pass
    return 0


async def cmd_feeds(args) -> int:
    """Состояние лент новых сообщений."""
    from . import feeds

    settings = load_settings()
    if args.poll:
        print("добавлено:", await feeds.poll_once(settings) or "ничего нового")
    for rule in settings.chats.values():
        if rule.read:
            path = feeds.feed_path(settings, rule.alias)
            print(f"  {rule.alias:<28} последний id {feeds.last_id(path):<10} {path}")
    return 0


async def cmd_pending(args) -> int:
    settings = load_settings()
    rows = approvals.pending_summary(settings)
    if not rows:
        print("Ничего не ждёт подтверждения.")
        return 0
    for row in rows:
        card = "карточка в боте" if row["card"] else "карточки нет"
        print(f"{row['id']} → {row['chat']}  ({card}, создан {row['created_at']})")
        for line in row["text"].splitlines() or [""]:
            print(f"    {line}")
        print()
    return 0


EXAMPLE_API_ID = 1234567


async def cmd_doctor(args) -> int:
    """Проверить настройку: .env, белый список и, если не --offline, саму сессию.

    Наличие файла сессии ничего не доказывает: Telethon создаёт его при любой
    попытке подключения, ещё до логина. Поэтому авторизацию спрашиваем у
    Telegram, а не у файла.
    """
    problems = []
    try:
        settings = load_settings()
    except ConfigError as exc:
        print(f"[x] {exc}")
        return 1
    print(f"[v] .env прочитан, api_id={settings.api_id}")
    if settings.api_id == EXAMPLE_API_ID:
        problems.append("в .env остались значения-заглушки из .env.example — впиши свои с my.telegram.org")

    if args.offline:
        print(f"[?] сессия: {settings.session_path} (проверка пропущена, --offline)")
    elif settings.api_id == EXAMPLE_API_ID:
        print("[?] сессия: не проверяю, пока не заданы настоящие ключи")
    else:
        try:
            async with tg.open_client(settings) as client:
                me = await client.get_me()
            print(f"[v] авторизован: {me.first_name or ''} @{me.username or '—'} (id {me.id})")
        except tg.NotLoggedIn:
            print(f"[x] сессия: {settings.session_path}")
            problems.append("аккаунт не подключён — выполни `tg login`")
        except Exception as exc:  # noqa: BLE001 — сюда попадают сетевые сбои
            print(f"[?] сессия: не удалось проверить ({type(exc).__name__}: {exc})")

    print(f"[v] политика отправки: {settings.send_policy}")

    if settings.send_policy == "bot_approval":
        if args.offline:
            print(f"[?] бот подтверждений: chat {settings.approval_chat_id} (не проверяю, --offline)")
        else:
            try:
                me = await bot.call(settings, "getMe")
                print(f"[v] бот подтверждений: @{me['username']} → chat {settings.approval_chat_id}")
            except bot.BotError as exc:
                print(f"[x] бот подтверждений: {exc}")
                problems.append("бот недоступен — кнопки подтверждения работать не будут")
        listener = FileLock(settings.updates_lock_path, timeout=0.2)
        try:
            with listener:
                print("[?] слушатель кнопок не запущен — нажатие сработает, только пока агент ждёт")
        except SessionBusy:
            print("[v] слушатель кнопок работает (`tg approvals`)")

    print(f"[v] чатов в белом списке: {len(settings.chats)}")
    for rule in settings.chats.values():
        flags = ("r" if rule.read else "-") + ("w" if rule.send else "-")
        print(f"    [{flags}] {rule.alias:<16} {rule.peer}  {rule.title}")
    if not settings.chats:
        problems.append("белый список пуст — агенту нечего читать")
    for line in problems:
        print(f"[x] {line}")
    return 1 if problems else 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="tg", description="Доступ агентов к Telegram")
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("login", help="войти в аккаунт и сохранить сессию")
    p.add_argument("--phone")
    p.set_defaults(func=cmd_login)

    sub.add_parser("logout", help="отозвать сессию").set_defaults(func=cmd_logout)
    sub.add_parser("whoami", help="под каким аккаунтом работаем").set_defaults(func=cmd_whoami)

    p = sub.add_parser("dialogs", help="все диалоги аккаунта (для заполнения белого списка)")
    p.add_argument("--limit", type=int, default=200)
    p.add_argument("--filter", help="подстрока в названии")
    p.set_defaults(func=cmd_dialogs)

    p = sub.add_parser("allow", help="добавить чат в белый список")
    p.add_argument("alias", help="короткое имя, которым к чату будет обращаться агент")
    p.add_argument("--id", help="id из `tg dialogs`, @username или me")
    p.add_argument("--match", help="вместо --id: найти чат по куску названия")
    p.add_argument("--title", help="человекочитаемое название")
    p.add_argument("--send", action="store_true", help="разрешить отправку (по умолчанию только чтение)")
    p.add_argument("--no-read", action="store_true", help="запретить чтение")
    p.add_argument("--limit", type=int, default=300, help="сколько диалогов просматривать при --match")
    p.set_defaults(func=cmd_allow)

    p = sub.add_parser("chats", help="что разрешено агентам")
    p.add_argument("--status", action="store_true", help="добавить непрочитанные")
    p.set_defaults(func=cmd_chats)

    p = sub.add_parser("read", help="прочитать разрешённый чат")
    p.add_argument("chat")
    p.add_argument("--limit", type=int, default=30)
    p.add_argument("--search")
    p.set_defaults(func=cmd_read)

    p = sub.add_parser("drafts", help="очередь черновиков")
    p.add_argument("--status", choices=["pending", "approved", "sent", "cancelled", "expired"])
    p.add_argument("--json", action="store_true")
    p.set_defaults(func=cmd_drafts)

    p = sub.add_parser("approve", help="подтвердить черновик агента")
    p.add_argument("draft_id")
    p.add_argument("--send", action="store_true", help="сразу и отправить")
    p.set_defaults(func=cmd_approve)

    p = sub.add_parser("reject", help="отменить черновик")
    p.add_argument("draft_id")
    p.set_defaults(func=cmd_reject)

    p = sub.add_parser("send", help="отправить подтверждённый черновик вручную")
    p.add_argument("draft_id")
    p.set_defaults(func=cmd_send)

    p = sub.add_parser("approvals", help="слушать кнопки подтверждения в боте")
    p.add_argument("--once", action="store_true", help="разобрать накопившиеся нажатия и выйти")
    p.set_defaults(func=cmd_approvals)

    sub.add_parser("pending", help="что ждёт нажатия кнопки").set_defaults(func=cmd_pending)

    p = sub.add_parser("watch", help="подписка: «alias id» на каждое новое сообщение")
    p.add_argument("chats", nargs="*", help="алиасы; без них — все разрешённые на чтение")
    p.set_defaults(func=cmd_watch)

    p = sub.add_parser("feeds", help="ленты новых сообщений")
    p.add_argument("--poll", action="store_true", help="сделать один проход прямо сейчас")
    p.set_defaults(func=cmd_feeds)

    p = sub.add_parser("doctor", help="проверить конфигурацию")
    p.add_argument("--offline", action="store_true", help="не обращаться к Telegram")
    p.set_defaults(func=cmd_doctor)
    return parser


def main() -> None:
    logging.getLogger("telethon").setLevel(logging.ERROR)  # mtproto шумит в stderr
    args = build_parser().parse_args()
    try:
        sys.exit(asyncio.run(args.func(args)) or 0)
    except (ConfigError, PermissionError, KeyError, ValueError) as exc:
        print(f"Ошибка: {exc}", file=sys.stderr)
        sys.exit(1)
    except KeyboardInterrupt:
        sys.exit(130)


if __name__ == "__main__":
    main()
