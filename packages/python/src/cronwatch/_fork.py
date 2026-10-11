"""What a forked child (gunicorn, Celery's prefork pool, multiprocessing's
fork start method) must not inherit: the locks of clients and stores, which
another thread of the parent may have held at the fork and which nothing in
the child would ever release, and the stores' connections, whose sockets and
SQLite handles belong to the parent.

Each client and store is tracked here, weakly, and replaces them in the
child through ``os.register_at_fork(after_in_child=...)``, while the child
still has only the thread that forked. Each also keeps the pid it last
reset for and checks it on use, for a fork the hook did not see (one made
from C); that reset replaces its locks and connection under ``lock`` and
writes the pid last, so a second thread never sees the new pid beside the
parent's locks."""

from __future__ import annotations

import os
import threading
import weakref
from typing import Any, Protocol

__all__ = ["lock", "track"]


class _Resettable(Protocol):
    def _after_fork(self) -> None: ...


#: Held while a pid check resets an object, so two threads of a child reset it once.
lock = threading.Lock()
_tracked: weakref.WeakSet[Any] = weakref.WeakSet()


def track(item: _Resettable) -> None:
    """Resets `item` (its ``_after_fork()``) in every child forked from now on."""
    with lock:
        _tracked.add(item)


def _in_child() -> None:
    global lock
    # The parent's lock may have been held by another thread at the fork.
    lock = threading.Lock()
    for item in list(_tracked):
        try:
            item._after_fork()
        except Exception:  # noqa: BLE001, one object must not stop the rest, or the fork
            pass


if hasattr(os, "register_at_fork"):
    os.register_at_fork(after_in_child=_in_child)
