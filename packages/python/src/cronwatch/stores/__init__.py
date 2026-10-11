"""Where jobs, runs, and state live.

Every store answers the same methods, the SDK's Store in snake_case:
``init`` (optional), ``upsert_job``, ``get_job``, ``list_jobs``,
``delete_job``, ``insert_run``, ``update_run``, ``update_run_if``
(optional: without it the client reads the run, then writes it), ``get_run``,
``list_runs``, ``last_run`` (optional: the client never calls it; the built-in
stores keep it for callers of their own), ``running_runs``, ``get_state``,
``set_state``, ``compare_and_set_state`` (optional: without it the client
falls back to ``set_state``), ``prune``, and ``close`` (optional). They take
and return the types in ``cronwatch.types``. A store without
``update_run_if`` or ``compare_and_set_state`` is safe only while one
process at a time uses it, and the client reports that once, through
on_error, when it first uses the store.
"""

from __future__ import annotations

from typing import Protocol, runtime_checkable

from ..types import JobDefinition, JobState, Run, StoredJob
from .memory import MemoryStore
from .sqlite import SqliteStore


@runtime_checkable
class Store(Protocol):
    def upsert_job(self, definition: JobDefinition, now: int) -> None: ...
    def get_job(self, name: str) -> StoredJob | None: ...
    def list_jobs(self) -> list[StoredJob]: ...
    def delete_job(self, name: str) -> None: ...
    def insert_run(self, run: Run) -> None: ...
    def update_run(self, run: Run) -> None: ...
    def get_run(self, run_id: str) -> Run | None: ...
    def list_runs(self, job: str, limit: int) -> list[Run]: ...
    def running_runs(self) -> list[Run]: ...
    def get_state(self, job: str) -> JobState | None: ...
    def set_state(self, state: JobState) -> None: ...
    def prune(self, before: int) -> int: ...


__all__ = ["MemoryStore", "SqliteStore", "Store"]
