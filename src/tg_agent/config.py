from __future__ import annotations

import os
import tomllib
from dataclasses import dataclass, field
from pathlib import Path

from dotenv import load_dotenv

ROOT = Path(os.environ.get("TG_AGENT_HOME") or Path(__file__).resolve().parents[2])


class ConfigError(RuntimeError):
    """Проблема в .env или chats.toml — сообщение адресовано человеку."""


@dataclass(frozen=True)
class ChatRule:
    alias: str
    peer: int | str
    title: str
    read: bool = True
    send: bool = False
    note: str = ""

    def as_dict(self) -> dict:
        return {
            "alias": self.alias,
            "title": self.title,
            "can_read": self.read,
            "can_send": self.send,
            "note": self.note,
        }


@dataclass(frozen=True)
class Settings:
    api_id: int
    api_hash: str
    phone: str
    session_path: Path
    send_policy: str
    draft_ttl_min: int
    proxy: tuple | None
    max_limit: int
    bot_token: str = ""
    approval_chat_id: int = 0
    bot_proxy: str = ""
    approval_timeout_sec: int = 600
    transcribe_per_call: int = 10
    max_file_mb: int = 200
    max_download_mb: int = 1024
    reactions: bool = True
    feed_interval: float = 10.0
    chats: dict[str, ChatRule] = field(default_factory=dict)

    @property
    def data_dir(self) -> Path:
        return ROOT / "data"

    @property
    def bot_ready(self) -> bool:
        return bool(self.bot_token and self.approval_chat_id)

    @property
    def updates_lock_path(self) -> Path:
        return self.data_dir / "bot-updates"

    @property
    def downloads_dir(self) -> Path:
        return self.data_dir / "downloads"

    @property
    def transcripts_path(self) -> Path:
        return self.data_dir / "transcripts.json"

    @property
    def offset_path(self) -> Path:
        return self.data_dir / "bot_offset.json"

    @property
    def outbox_path(self) -> Path:
        return self.data_dir / "outbox.json"

    @property
    def audit_path(self) -> Path:
        return self.data_dir / "audit.jsonl"

    def resolve(self, chat: str) -> ChatRule:
        """Найти чат в белом списке по alias, id или username. Иначе — отказ."""
        key = (chat or "").strip()
        if not key:
            raise PermissionError("Не указан чат.")
        low = key.lower()
        by_alias = {a.lower(): r for a, r in self.chats.items()}
        if low in by_alias:
            return by_alias[low]
        for rule in self.chats.values():
            peer = str(rule.peer).lower()
            if low == peer or low.lstrip("@") == peer.lstrip("@"):
                return rule
        known = ", ".join(sorted(self.chats)) or "(список пуст)"
        raise PermissionError(
            f"Чат '{chat}' не в белом списке, доступ запрещён. "
            f"Разрешённые алиасы: {known}. "
            "Добавить чат может только человек — правкой config/chats.toml."
        )


def _parse_proxy(raw: str | None) -> tuple | None:
    if not raw:
        return None
    import socks  # noqa: PLC0415 — идёт вместе с telethon, нужен только при прокси

    url = raw.strip()
    scheme, _, rest = url.partition("://")
    if not rest:
        scheme, rest = "socks5", url
    kinds = {"socks5": socks.SOCKS5, "socks4": socks.SOCKS4, "http": socks.HTTP}
    if scheme not in kinds:
        raise ConfigError(f"TG_PROXY: неизвестная схема '{scheme}'")
    host, _, port = rest.rpartition(":")
    if not host or not port.isdigit():
        raise ConfigError("TG_PROXY: ожидается вид socks5://host:port")
    return (kinds[scheme], host, int(port))


def load_chats(path: Path | None = None) -> dict[str, ChatRule]:
    path = path or ROOT / "config" / "chats.toml"
    if not path.exists():
        raise ConfigError(
            f"Нет файла {path}. Скопируй config/chats.example.toml в config/chats.toml "
            "и перечисли разрешённые чаты."
        )
    raw = tomllib.loads(path.read_text(encoding="utf-8"))
    rules: dict[str, ChatRule] = {}
    for entry in raw.get("chat", []):
        alias = str(entry.get("alias", "")).strip()
        if not alias:
            raise ConfigError(f"{path}: у одной из записей [[chat]] нет alias")
        if "id" not in entry:
            raise ConfigError(f"{path}: у чата '{alias}' нет поля id")
        if alias.lower() in {a.lower() for a in rules}:
            raise ConfigError(f"{path}: alias '{alias}' повторяется")
        rules[alias] = ChatRule(
            alias=alias,
            peer=entry["id"],
            title=str(entry.get("title", alias)),
            read=bool(entry.get("read", True)),
            send=bool(entry.get("send", False)),
            note=str(entry.get("note", "")),
        )
    return rules


def _bot_credentials() -> tuple[str, int]:
    """Токен бота подтверждений.

    Свой бот заводить не нужно: по умолчанию берём тот же, что шлёт
    уведомления о хуках Claude Code, прямо из его config.env — чтобы токен
    жил в одном месте и переживал ротацию без правок тут.
    """
    token = os.environ.get("TG_BOT_TOKEN", "").strip()
    chat_id = os.environ.get("TG_APPROVAL_CHAT_ID", "").strip()
    if token and chat_id:
        return token, int(chat_id)

    donor = Path(
        os.environ.get("TG_BOT_ENV_FILE")
        or Path.home() / ".local" / "share" / "cc-telegram-notify" / "config.env"
    )
    if donor.exists():
        for line in donor.read_text(encoding="utf-8", errors="replace").splitlines():
            key, _, value = line.partition("=")
            key, value = key.strip(), value.strip().strip("'\"")
            if key == "NOTIFICATIONS_BOT_TOKEN" and not token:
                token = value
            elif key == "NOTIFICATIONS_CHAT_ID" and not chat_id:
                chat_id = value
    return token, int(chat_id) if chat_id.lstrip("-").isdigit() else 0


def load_settings(*, require_credentials: bool = True) -> Settings:
    load_dotenv(ROOT / ".env")
    api_id = os.environ.get("TG_API_ID", "").strip()
    api_hash = os.environ.get("TG_API_HASH", "").strip()
    if require_credentials and (not api_id or not api_hash):
        raise ConfigError(
            "Не заданы TG_API_ID / TG_API_HASH. Скопируй .env.example в .env "
            "и впиши значения с https://my.telegram.org"
        )
    if api_id and not api_id.isdigit():
        raise ConfigError("TG_API_ID должен быть числом")

    session = Path(os.environ.get("TG_SESSION", "data/user.session"))
    if not session.is_absolute():
        session = ROOT / session
    session.parent.mkdir(parents=True, exist_ok=True)

    policy = os.environ.get("TG_SEND_POLICY", "bot_approval").strip().lower()
    if policy not in {"bot_approval", "human_approval", "agent_confirm", "disabled"}:
        raise ConfigError(f"TG_SEND_POLICY: недопустимое значение '{policy}'")

    bot_token, approval_chat_id = _bot_credentials()
    if policy == "bot_approval" and not (bot_token and approval_chat_id):
        raise ConfigError(
            "TG_SEND_POLICY=bot_approval, но не найден бот для подтверждений. "
            "Задай TG_BOT_TOKEN и TG_APPROVAL_CHAT_ID в .env либо путь к чужому "
            "config.env в TG_BOT_ENV_FILE."
        )

    return Settings(
        api_id=int(api_id or 0),
        api_hash=api_hash,
        phone=os.environ.get("TG_PHONE", "").strip(),
        session_path=session,
        send_policy=policy,
        draft_ttl_min=int(os.environ.get("TG_DRAFT_TTL_MIN", "60")),
        proxy=_parse_proxy(os.environ.get("TG_PROXY")),
        max_limit=int(os.environ.get("TG_MAX_LIMIT", "200")),
        bot_token=bot_token,
        approval_chat_id=approval_chat_id,
        bot_proxy=os.environ.get("TG_BOT_PROXY", "").strip(),
        approval_timeout_sec=int(os.environ.get("TG_APPROVAL_TIMEOUT_SEC", "600")),
        transcribe_per_call=int(os.environ.get("TG_TRANSCRIBE_PER_CALL", "10")),
        max_file_mb=int(os.environ.get("TG_MAX_FILE_MB", "200")),
        max_download_mb=int(os.environ.get("TG_MAX_DOWNLOAD_MB", "1024")),
        reactions=os.environ.get("TG_REACTIONS", "direct").strip().lower() != "off",
        feed_interval=float(os.environ.get("TG_FEED_INTERVAL", "10")),
        chats=load_chats(),
    )
