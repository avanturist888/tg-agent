from __future__ import annotations

import errno
import json
import os
import time
from pathlib import Path

STALE_AFTER_SEC = 120

# Локи, которые этот процесс держит прямо сейчас. Нужны, чтобы отличить свой
# живой лок от своего же осиротевшего: pid в файле совпадает в обоих случаях.
_HELD: set[str] = set()


class SessionBusy(RuntimeError):
    pass


def _pid_alive(pid: int, since: float | None = None) -> bool:
    """Жив ли процесс. НЕ через os.kill(pid, 0): на Windows это TerminateProcess.

    since — когда владелец взял лок. Windows быстро раздаёт pid умерших
    процессов заново; процесс, запущенный позже взятия лока, — не владелец.
    """
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
        if code.value != 259:  # STILL_ACTIVE
            return False
        if since is None:
            return True
        created, _exit, _kernel, _user = (ctypes.c_ulonglong() for _ in range(4))
        if not kernel32.GetProcessTimes(
            handle, ctypes.byref(created), ctypes.byref(_exit), ctypes.byref(_kernel), ctypes.byref(_user)
        ):
            return True
        # FILETIME: сотни наносекунд от 1601-01-01
        started = created.value / 10_000_000 - 11_644_473_600
        return started <= since + 1
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
        _HELD.add(str(self.path))
        return True

    def _break_if_stale(self) -> None:
        try:
            age = time.time() - self.path.stat().st_mtime
            meta = json.loads(self.path.read_text(encoding="utf-8") or "{}")
            owner, taken = meta.get("pid"), meta.get("ts")
        except (FileNotFoundError, ValueError, OSError):
            return
        # Протухшим считаем только лок мёртвого владельца. Живой процесс может
        # законно держать сессию долго (скачивание видео, чтение с расшифровкой),
        # и сносить его лок по возрасту нельзя: так уже падала отправка.
        # Возраст решает, лишь если владельца не записали (упал между
        # созданием файла и записью pid).
        if owner and int(owner) == os.getpid():
            # Наш pid, но мы его не держим — снять файл при выходе не удалось.
            # Раньше такой лок считался живым навсегда, и процесс ждал сам себя.
            stale = str(self.path) not in _HELD
        else:
            stale = (not _pid_alive(int(owner), taken)) if owner else age > STALE_AFTER_SEC
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

    async def acquire_async(self) -> FileLock:
        """То же, что __enter__, но ждёт через await, не замораживая процесс.

        В одном процессе живут несколько задач (слушатель кнопок + опрос
        лент). Синхронный sleep, пока держатель лока — соседняя задача,
        блокировал её же и давал взаимную блокировку на весь таймаут.
        """
        import asyncio

        deadline = time.monotonic() + self.timeout
        while True:
            if self._try_acquire():
                return self
            if time.monotonic() > deadline:
                raise SessionBusy(
                    "Сессия Telegram занята другим процессом tg-agent "
                    f"(лок {self.path}). Подожди или удали лок-файл вручную."
                )
            await asyncio.sleep(0.25)

    def touch(self) -> None:
        """Продлить лок: демон держит его часами, а протухшим считается тот,
        чей файл не обновлялся STALE_AFTER_SEC секунд."""
        if self._fd is not None:
            self.path.touch()

    def __exit__(self, *exc) -> None:
        if self._fd is not None:
            os.close(self._fd)
            self._fd = None
        _HELD.discard(str(self.path))
        # Windows не даёт удалить файл, пока его читает другой процесс
        # (_break_if_stale у соседа) — это доли секунды, переждём. Падать нельзя:
        # работа под локом уже сделана (например, сообщение уже отправлено).
        for _ in range(40):
            try:
                self.path.unlink(missing_ok=True)
                return
            except PermissionError:
                time.sleep(0.05)
        # не вышло — файл останется с нашим pid; _HELD его уже не знает,
        # так что и мы, и другие (после нашей смерти) снимут его как протухший
