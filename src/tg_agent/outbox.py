from __future__ import annotations

import json
import secrets
from dataclasses import asdict, dataclass, field, fields
from datetime import datetime, timedelta, timezone
from pathlib import Path

from .lock import FileLock

PENDING = "pending"
APPROVED = "approved"
SENT = "sent"
CANCELLED = "cancelled"
EXPIRED = "expired"


@dataclass
class Draft:
    id: str
    chat: str
    text: str
    reply_to: int | None
    created_at: str
    expires_at: str
    status: str = PENDING
    origin: str = "agent"
    approved_at: str | None = None
    sent_at: str | None = None
    message_id: int | None = None
    bot_message_id: int | None = None
    fmt: str = "markdown"
    files: list[dict] = field(default_factory=list)  # снимки вложений: path, name, size
    send_error: str | None = None  # одобрено, но отправка сорвалась — текст ошибки
    note: str = ""
    history: list[dict] = field(default_factory=list)

    def is_expired(self, now: datetime | None = None) -> bool:
        now = now or datetime.now(timezone.utc)
        return self.status in {PENDING, APPROVED} and now > datetime.fromisoformat(self.expires_at)


class Outbox:
    """Очередь черновиков на отправку.

    Агент кладёт сюда текст, человек подтверждает. Файл общий для всех
    процессов, поэтому любые изменения — под локом.
    """

    def __init__(self, path: Path):
        self.path = path
        self.path.parent.mkdir(parents=True, exist_ok=True)

    def _read(self) -> list[Draft]:
        if not self.path.exists():
            return []
        raw = json.loads(self.path.read_text(encoding="utf-8") or "[]")
        # Файл общий для процессов разных версий: MCP-сервер живёт, пока открыта
        # сессия Claude Code, и не видит свежего кода. Незнакомые поля от более
        # новой версии пропускаем, иначе старый процесс не прочтёт ни одного
        # черновика (так уже было с полем fmt).
        # Незнакомые поля не выбрасываем, а возвращаем при записи — иначе старый
        # процесс, переписав файл, молча стёр бы данные новых черновиков.
        known = {f.name for f in fields(Draft)}
        drafts = []
        for item in raw:
            draft = Draft(**{k: v for k, v in item.items() if k in known})
            draft.__dict__["_extra"] = {k: v for k, v in item.items() if k not in known}
            drafts.append(draft)
        return drafts

    def _write(self, drafts: list[Draft]) -> None:
        payload = json.dumps(
            [{**d.__dict__.get("_extra", {}), **asdict(d)} for d in drafts], ensure_ascii=False, indent=2
        )
        tmp = self.path.with_suffix(".tmp")
        tmp.write_text(payload, encoding="utf-8")
        tmp.replace(self.path)

    def _touch_expired(self, drafts: list[Draft]) -> list[Draft]:
        for draft in drafts:
            if draft.is_expired():
                draft.status = EXPIRED
        return drafts

    @staticmethod
    def new_id() -> str:
        return f"{datetime.now(timezone.utc):%m%d-%H%M}-{secrets.token_hex(2)}"

    def create(
        self,
        *,
        chat: str,
        text: str,
        reply_to: int | None,
        ttl_min: int,
        note: str = "",
        fmt: str = "markdown",
        files: list[dict] | None = None,
        draft_id: str | None = None,
    ) -> Draft:
        now = datetime.now(timezone.utc)
        draft = Draft(
            id=draft_id or self.new_id(),
            chat=chat,
            text=text,
            reply_to=reply_to,
            created_at=now.isoformat(timespec="seconds"),
            expires_at=(now + timedelta(minutes=ttl_min)).isoformat(timespec="seconds"),
            note=note,
            fmt=fmt,
            files=files or [],
        )
        with FileLock(self.path):
            drafts = self._touch_expired(self._read())
            drafts.append(draft)
            self._write(drafts[-200:])
        return draft

    def list(self, *, status: str | None = None) -> list[Draft]:
        with FileLock(self.path):
            drafts = self._touch_expired(self._read())
            self._write(drafts)
        return [d for d in drafts if status is None or d.status == status]

    def get(self, draft_id: str) -> Draft:
        for draft in self.list():
            if draft.id == draft_id:
                return draft
        raise KeyError(f"Черновик '{draft_id}' не найден")

    def _update(self, draft_id: str, mutate) -> Draft:
        with FileLock(self.path):
            drafts = self._touch_expired(self._read())
            for draft in drafts:
                if draft.id == draft_id:
                    mutate(draft)
                    self._write(drafts)
                    return draft
            raise KeyError(f"Черновик '{draft_id}' не найден")

    def approve(self, draft_id: str, by: str = "human") -> Draft:
        def mutate(draft: Draft) -> None:
            if draft.status != PENDING:
                raise ValueError(f"Черновик {draft_id} в статусе '{draft.status}', подтвердить нельзя")
            draft.status = APPROVED
            draft.approved_at = datetime.now(timezone.utc).isoformat(timespec="seconds")
            draft.history.append({"event": "approved", "by": by, "at": draft.approved_at})

        return self._update(draft_id, mutate)

    def cancel(self, draft_id: str, by: str = "human") -> Draft:
        def mutate(draft: Draft) -> None:
            if draft.status in {SENT, CANCELLED}:
                raise ValueError(f"Черновик {draft_id} уже в статусе '{draft.status}'")
            draft.status = CANCELLED
            draft.history.append(
                {"event": "cancelled", "by": by, "at": datetime.now(timezone.utc).isoformat(timespec="seconds")}
            )

        return self._update(draft_id, mutate)

    def attach_card(self, draft_id: str, message_id: int) -> Draft:
        """Запомнить карточку в боте, чтобы потом переписать её итогом."""

        def mutate(draft: Draft) -> None:
            draft.bot_message_id = message_id

        return self._update(draft_id, mutate)

    def mark_send_error(self, draft_id: str, error: str) -> Draft:
        def mutate(draft: Draft) -> None:
            draft.send_error = error[:500]
            draft.history.append(
                {"event": "send_failed", "at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
                 "error": error[:300]}
            )

        return self._update(draft_id, mutate)

    def mark_sent(self, draft_id: str, message_id: int, at: str) -> Draft:
        def mutate(draft: Draft) -> None:
            draft.status = SENT
            draft.send_error = None
            draft.sent_at = at
            draft.message_id = message_id
            draft.history.append({"event": "sent", "at": at, "message_id": message_id})

        return self._update(draft_id, mutate)
