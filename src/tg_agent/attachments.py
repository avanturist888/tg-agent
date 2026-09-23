"""Файлы, которые агент хочет отправить.

При создании черновика файл копируется в data/outbox_files/<draft_id>/:
человек одобряет именно этот снимок, и именно он уходит. Иначе агент мог бы
подменить содержимое между нажатием кнопки и отправкой.
"""

from __future__ import annotations

import fnmatch
import shutil
from pathlib import Path

from .config import ROOT, Settings

# Секреты не уезжают в Telegram даже с одобрения: в карточке их легко не заметить.
DENY_NAMES = [
    ".env", ".env.*", "*.session", "*.session-journal", "id_rsa*", "id_ed25519*", "id_ecdsa*",
    "*.pem", "*.key", "*.p12", "*.pfx", "*.kdbx", "credentials*", ".netrc", ".git-credentials",
]
DENY_DIRS = {".ssh", ".gnupg", ".aws", ".azure", ".kube", ".docker"}


def _denied_reason(path: Path) -> str | None:
    if path.is_relative_to(ROOT):
        return "файлы самого tg-agent (сессия, .env, очередь) не отправляются"
    if DENY_DIRS & {part.lower() for part in path.parts}:
        return "файлы из каталогов с ключами и учётками не отправляются"
    name = path.name.lower()
    if any(fnmatch.fnmatch(name, pattern) for pattern in DENY_NAMES):
        return "похоже на секрет (ключ, сессия, .env) — такое не отправляется"
    return None


def snapshot(settings: Settings, draft_id: str, paths: list[str]) -> list[dict]:
    """Проверить файлы и снять с них копии для черновика."""
    if len(paths) > 10:
        raise ValueError("Не больше 10 файлов в одном сообщении (так группирует Telegram).")
    limit = settings.max_file_mb * 1024 * 1024
    checked: list[tuple[Path, int]] = []
    for raw in paths:
        path = Path(raw).expanduser().resolve()
        if not path.is_file():
            raise ValueError(f"Файл не найден: {raw}")
        reason = _denied_reason(path)
        if reason:
            raise PermissionError(f"{path.name}: {reason}.")
        size = path.stat().st_size
        if size == 0:
            raise ValueError(f"Файл пустой: {path.name}")
        if size > limit:
            raise ValueError(f"{path.name}: {size // (1024 * 1024)} МБ, предел {settings.max_file_mb} МБ.")
        checked.append((path, size))

    target = settings.data_dir / "outbox_files" / draft_id
    target.mkdir(parents=True, exist_ok=True)
    result = []
    for i, (path, size) in enumerate(checked):
        # своя подпапка на файл: имя остаётся исходным — Telegram берёт его
        # из пути, а одноимённые файлы из разных мест не затрут друг друга
        slot = target / str(i)
        slot.mkdir(exist_ok=True)
        copy = slot / path.name
        shutil.copy2(path, copy)
        result.append({"path": str(copy), "name": path.name, "size": size, "source": str(path)})
    return result


def drop(settings: Settings, draft_id: str) -> None:
    """Убрать снимки, когда черновик отправлен или отменён."""
    shutil.rmtree(settings.data_dir / "outbox_files" / draft_id, ignore_errors=True)


def human_size(size: int) -> str:
    for unit in ("Б", "КБ", "МБ"):
        if size < 1024:
            return f"{size:.0f} {unit}"
        size /= 1024
    return f"{size:.1f} ГБ"
