"""A forked child gets locks and connections of its own, even when another
thread of the parent held one at the fork (cronwatch._fork)."""

from __future__ import annotations

import os
import signal
import threading
from collections.abc import Callable, Iterator
from contextlib import contextmanager
from pathlib import Path
from typing import Any

import pytest

from cronwatch import Cronwatch, Run, _fork
from cronwatch.stores import MemoryStore, SqliteStore

from helpers import PG, NO_PG, drop_pg_tables, pg_prefix

needs_fork = pytest.mark.skipif(not hasattr(os, "fork") or not hasattr(signal, "alarm"), reason="needs fork and alarm")
quiet = pytest.mark.filterwarnings("ignore:This process .* is multi-threaded, use of fork:DeprecationWarning")


@contextmanager
def held(*locks: Any) -> Iterator[None]:
    """Each lock held by a thread of its own until the block ends, as a busy app's threads hold them at a fork."""
    taken = threading.Barrier(len(locks) + 1)
    done = threading.Event()

    def hold(lock: Any) -> None:
        with lock:
            taken.wait(5)
            done.wait(10)

    threads = [threading.Thread(target=hold, args=(lock,), daemon=True) for lock in locks]
    for thread in threads:
        thread.start()
    taken.wait(5)
    try:
        yield
    finally:
        done.set()
        for thread in threads:
            thread.join(5)


def in_child(work: Callable[[], bool]) -> int:
    """Runs `work` in a forked child, which an alarm ends after five seconds
    (a deadlock); its exit code: 0 when it returned True."""
    pid = os.fork()
    if pid == 0:  # pragma: no cover, the child
        signal.alarm(5)
        code = 1
        try:
            code = 0 if work() else 3
        except BaseException:  # noqa: BLE001
            code = 2
        os._exit(code)
    _, status = os.waitpid(pid, 0)
    return os.waitstatus_to_exitcode(status)


def run(store: Any, run_id: str) -> Run:
    return Run(id=run_id, job="j", status="ok", started_at=1, finished_at=2, duration_ms=1)


@needs_fork
@quiet
def test_a_child_uses_a_sqlite_store_whose_lock_a_parent_thread_held_at_the_fork(tmp_path: Path) -> None:
    store = SqliteStore(str(tmp_path / "fork.db"))
    store.init()
    parent_connection = store._db
    with held(store._mutex):
        code = in_child(lambda: (store.insert_run(run(store, "child")), store._db is not parent_connection, store._pid == os.getpid())[1:] == (True, True))
    assert code == 0
    assert store._db is parent_connection, "the parent keeps its connection"
    assert store.get_run("child") is not None


@needs_fork
@quiet
def test_a_child_uses_a_memory_store_whose_lock_a_parent_thread_held_at_the_fork() -> None:
    store = MemoryStore()
    store.insert_run(run(store, "parent"))
    with held(store._lock):
        code = in_child(lambda: (store.insert_run(run(store, "child")), store.get_run("parent") is not None)[1])
    assert code == 0
    assert store.get_run("child") is None, "the child's store is a copy"


@needs_fork
@quiet
def test_a_child_runs_a_job_while_a_parent_thread_held_the_clients_locks_at_the_fork(tmp_path: Path) -> None:
    store = SqliteStore(str(tmp_path / "fork.db"))
    cw = Cronwatch(store=store, alerts=[], cron_secret=None)
    cw.job("j", schedule="every 1h")
    cw.run("j", lambda ctx: None)
    with held(cw._registry, cw._serial("j"), cw._check_lock, store._mutex):

        def work() -> bool:
            assert cw.run("j", lambda ctx: "from the child") == "from the child"
            cw.check()
            return cw._pid == os.getpid()

        code = in_child(work)
    assert code == 0
    assert [r.output for r in cw.runs("j")][:1] == ["from the child"]


@needs_fork
@quiet
@pytest.mark.skipif(not PG, reason=NO_PG)
def test_a_child_opens_a_postgres_connection_of_its_own_and_never_closes_the_parents() -> None:
    from cronwatch.stores.postgres import PostgresStore

    prefix = pg_prefix("k")
    store = PostgresStore(PG, prefix=prefix)
    try:
        store.init()
        parent_connection = store._connection
        with held(store._lock):

            def work() -> bool:
                store.insert_run(run(store, "child"))
                fresh = store._connection is not parent_connection
                store.close()
                return fresh

            code = in_child(work)
        assert code == 0
        assert store._connection is parent_connection and not parent_connection.closed
        assert store.get_run("child") is not None, "the parent's connection still works"
    finally:
        store.close()
        drop_pg_tables(prefix)


def test_a_fork_the_hook_did_not_see_is_caught_by_the_pid_and_reset_once(tmp_path: Path) -> None:
    """The fallback: a pid that is not this process's replaces the locks and
    connection first and writes the pid last, under _fork.lock, once however
    many threads notice it."""
    store = SqliteStore(str(tmp_path / "pid.db"))
    store.init()
    old_lock, old_connection = store._mutex, store._db
    resets: list[int] = []
    real = store._after_fork

    def counted() -> None:
        resets.append(1)
        # The pid is still the old one while the locks are replaced.
        assert store._pid == -1
        real()

    store._after_fork = counted  # type: ignore[method-assign]
    store._pid = -1
    with held(old_lock):
        start = threading.Barrier(4)

        def use() -> None:
            start.wait(5)
            store.insert_run(run(store, f"t{threading.get_ident()}"))

        threads = [threading.Thread(target=use) for _ in range(4)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(5)
            assert not thread.is_alive(), "a thread waited on the old lock"
    assert resets == [1]
    assert store._pid == os.getpid() and store._mutex is not old_lock and store._db is not old_connection
    old_connection.close()  # type: ignore[union-attr]
    cw = Cronwatch(store=MemoryStore(), alerts=[], cron_secret=None)
    cw.job("j")
    check_lock = cw._check_lock
    cw._pid = -1
    with held(check_lock):
        cw.check()
    assert cw._check_lock is not check_lock and cw._pid == os.getpid()


def test_clients_and_stores_are_tracked_weakly() -> None:
    import gc
    import weakref

    store = MemoryStore()
    ref = weakref.ref(store)
    assert store in _fork._tracked
    del store
    gc.collect()
    assert ref() is None
