from __future__ import annotations

import errno
import json
import os
import time
from pathlib import Path

STALE_AFTER_SEC = 120


class SessionBusy(RuntimeError):
    pass


def _pid_alive(pid: int) -> bool:
    """Жив ли процесс. НЕ через os.kill(pid, 0): на Windows это TerminateProcess."""
    if pid == os.getpid():
        return True
    if os.name != "nt":
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            return False
        except PermissionError:
            return True
        return True

    import ctypes

    kernel32 = ctypes.windll.kernel32
    handle = kernel32.OpenProcess(0x1000, False, pid)  # PROCESS_QUERY_LIMITED_INFORMATION
    if not handle:
        return False
    try:
        code = ctypes.c_ulong()
        kernel32.GetExitCodeProcess(handle, ctypes.byref(code))
        return code.value == 259  # STILL_ACTIVE
    finally:
        kernel32.CloseHandle(handle)


class FileLock:
    """Межпроцессный лок на файл сессии.

    Сессия Telethon — это SQLite, который не любит два одновременных
    подключения с одним auth key. MCP-сервер поднимается в каждом проекте
    отдельным процессом, поэтому доступ сериализуем локом, а соединение
    держим только на время запроса.
    """

    def __init__(self, path: Path, timeout: float = 45.0):
        self.path = Path(str(path) + ".lock")
        self.timeout = timeout
        self._fd: int | None = None

    def _try_acquire(self) -> bool:
        try:
            fd = os.open(self.path, os.O_CREAT | os.O_EXCL | os.O_RDWR)
        except OSError as exc:
            if exc.errno != errno.EEXIST:
                raise
            self._break_if_stale()
            return False
        os.write(fd, json.dumps({"pid": os.getpid(), "ts": time.time()}).encode())
        self._fd = fd
        return True

    def _break_if_stale(self) -> None:
        try:
            age = time.time() - self.path.stat().st_mtime
            owner = json.loads(self.path.read_text(encoding="utf-8") or "{}").get("pid")
        except (FileNotFoundError, ValueError, OSError):
            return
        # Протухшим считаем только лок мёртвого владельца. Живой процесс может
        # законно держать сессию долго (скачивание видео, чтение с расшифровкой),
        # и сносить его лок по возрасту нельзя: так уже падала отправка.
        # Возраст решает, лишь если владельца не записали (упал между
        # созданием файла и записью pid).
        stale = (not _pid_alive(int(owner))) if owner else age > STALE_AFTER_SEC
        if not stale:
            return
        try:
            self.path.unlink(missing_ok=True)
        except PermissionError:
            pass  # Windows: файл ещё открыт владельцем — значит, не протух

    def __enter__(self) -> FileLock:
        deadline = time.monotonic() + self.timeout
        while True:
            if self._try_acquire():
                return self
            if time.monotonic() > deadline:
                raise SessionBusy(
                    "Сессия Telegram занята другим процессом tg-agent "
                    f"(лок {self.path}). Подожди или удали лок-файл вручную."
                )
            time.sleep(0.25)

    def touch(self) -> None:
        """Продлить лок: демон держит его часами, а протухшим считается тот,
        чей файл не обновлялся STALE_AFTER_SEC секунд."""
        if self._fd is not None:
            self.path.touch()

    def __exit__(self, *exc) -> None:
        if self._fd is not None:
            os.close(self._fd)
            self._fd = None
        self.path.unlink(missing_ok=True)
