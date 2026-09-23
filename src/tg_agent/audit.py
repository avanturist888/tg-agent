from __future__ import annotations

import json
import os
from datetime import datetime, timezone
from pathlib import Path


def log(path: Path, action: str, **fields) -> None:
    """Дописать строку в журнал. Журнал — единственный источник правды о том,
    что агенты делали с аккаунтом."""
    record = {
        "ts": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "pid": os.getpid(),
        "action": action,
        **fields,
    }
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as fh:
        fh.write(json.dumps(record, ensure_ascii=False) + "\n")
