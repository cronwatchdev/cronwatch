"""Fixes from a review of this package: a request body that took the
standard library's email parser minutes, None as the opt-out, metrics that
are numbers but not int or float, a run left in another context than it
was entered in, generator functions as runs, and the Postgres store's
connection timeouts."""

from __future__ import annotations

import asyncio
import contextvars
import decimal
import fractions
import logging
import time
from typing import Any

import pytest

import cronwatch
from cronwatch import _env
from cronwatch.web import _MAX_BODY, Request, _multipart


def client(**options: Any) -> tuple[cronwatch.Cronwatch, list[tuple[str, str]]]:
    errors: list[tuple[str, str]] = []
    options.setdefault("alerts", [])
    return cronwatch.Cronwatch(on_error=lambda error, where: errors.append((where, str(error))), **options), errors


# ---------------------------------------------------------------- multipart


def _crafted() -> dict[str, bytes]:
    n = _MAX_BODY
    words = b'--b\r\nContent-Disposition: form-data; name="' + b"=?utf-8?q?a?= " * ((n - 100) // 14) + b'"\r\n\r\nx\r\n--b--\r\n'
    continuations = b'--b\r\nContent-Disposition: form-data; name*0="a"' + b"".join(b'; name*%d="a"' % i for i in range(1, 60000)) + b"\r\n\r\nx\r\n--b--\r\n"
    params = b"--b\r\nContent-Disposition: form-data" + b"; a=b" * ((n - 100) // 5) + b"\r\n\r\nx\r\n--b--\r\n"
    empty = b"--b\r\n\r\n" * (n // 7)
    small = b'--b\r\nContent-Disposition: form-data; name="a"\r\n\r\nx\r\n' * (n // 50)
    comments = b"--b\r\nContent-Disposition: " + b"(" * (n - 200) + b"\r\n\r\nx\r\n--b--\r\n"
    return {
        "encoded words": words[:n],
        "RFC 2231 continuations": continuations[:n],
        "many parameters": params[:n],
        "many empty parts": empty[:n],
        "many small parts": small[:n],
        "nested comments": comments[:n],
    }


@pytest.mark.parametrize("shape", list(_crafted()))
def test_a_crafted_multipart_sign_in_is_answered_at_once(shape: str, monkeypatch: pytest.MonkeyPatch) -> None:
    # Each of these held the email parser from 8 to 80 seconds at the 1 MiB cap, before any token was asked for.
    monkeypatch.setenv("CRONWATCH_ENV", "production")
    cw, _ = client()
    web = cw.routes(token="tok", base_path="/cronwatch")
    body = _crafted()[shape]
    assert len(body) <= _MAX_BODY
    started = time.perf_counter()
    answer = web.handle(Request("POST", "/cronwatch/signin", headers={"content-type": "multipart/form-data; boundary=b"}, body=body))
    took = time.perf_counter() - started
    assert answer.status in (400, 401)
    assert took < 2.0, f"{shape}: {took:.1f}s"


def test_multipart_fields_read_as_form_data_reads_them() -> None:
    kind = 'multipart/form-data; boundary="x y"'
    body = (
        b"preamble\r\n"
        b'--x y\r\nContent-Disposition: form-data; name="for"\r\n\r\n2h\r\n'
        b'--x y\r\ncontent-disposition: FORM-DATA; NAME="quo\\"ted"; x=1\r\n\r\nline one\r\nline two\r\n'
        b'--x y\r\nContent-Disposition: form-data; name="file"; filename="a.txt"\r\nContent-Type: text/plain\r\n\r\nhello\r\n'
        b'--x y\r\nContent-Disposition: attachment; name="other"\r\n\r\nno\r\n'
        b"--x y\r\nContent-Type: text/plain\r\n\r\nnameless\r\n"
        b'--x y\r\nContent-Disposition: form-data; name="bytes"\r\n\r\n\xff\xfe\r\n'
        b"--x y--\r\nepilogue"
    )
    assert _multipart(kind, body) == {"for": "2h", 'quo"ted': "line one\r\nline two", "file": "[object File]", "bytes": "��"}
    assert _multipart("multipart/form-data; boundary=b", b'--b\nContent-Disposition: form-data; name="a"\n\n1\n--b--') == {"a": "1"}
    for broken in (b"", b"no delimiter", b"--b\r\nContent-Disposition: form-data; name=a\r\n\r\nnever ends"):
        with pytest.raises(ValueError):
            _multipart("multipart/form-data; boundary=b", broken)
    with pytest.raises(ValueError):
        _multipart("multipart/form-data", body)


def test_a_multipart_sign_in_still_signs_in(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("CRONWATCH_ENV", "production")
    cw, _ = client()
    web = cw.routes(token="tok", base_path="/cronwatch")
    body = b'--b\r\nContent-Disposition: form-data; name="token"\r\n\r\ntok\r\n--b--\r\n'
    answer = web.handle(Request("POST", "/cronwatch/signin", headers={"content-type": "multipart/form-data; boundary=b"}, body=body))
    assert answer.status == 303 and "set-cookie" in answer.headers


# ---------------------------------------------------------------- None and OPEN


@pytest.fixture
def production(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("CRONWATCH_ENV", "production")
    monkeypatch.delenv("CRONWATCH_TOKEN", raising=False)
    monkeypatch.delenv("CRON_SECRET", raising=False)
    monkeypatch.setattr(_env, "_warned_open", set())


def _warnings(caplog: pytest.LogCaptureFixture) -> list[str]:
    return [record.getMessage() for record in caplog.records if record.name == "cronwatch" and "open to anyone" in record.getMessage()]


def test_routes_opened_by_none_warn_once_outside_development(production: None, caplog: pytest.LogCaptureFixture) -> None:
    cw, _ = client()
    with caplog.at_level(logging.WARNING, logger="cronwatch"):
        web = cw.routes(token=None)
        cw.routes(token=None)
    assert web.handle(Request("GET", "/api/jobs")).status == 200, "None still opts out"
    found = _warnings(caplog)
    assert len(found) == 1
    assert "token is None" in found[0] and "cronwatch.OPEN" in found[0] and "CRONWATCH_TOKEN" in found[0]


def test_routes_opened_by_name_do_not_warn(production: None, caplog: pytest.LogCaptureFixture) -> None:
    cw, _ = client()
    with caplog.at_level(logging.WARNING, logger="cronwatch"):
        web = cw.routes(token=cronwatch.OPEN)
        locked = cw.routes()
    assert web.token is None
    assert web.handle(Request("GET", "/api/jobs")).status == 200
    assert locked.handle(Request("GET", "/api/jobs")).status == 503, "not given at all is still locked"
    assert _warnings(caplog) == []
    assert repr(cronwatch.OPEN) == "cronwatch.OPEN"


def test_none_in_development_does_not_warn(production: None, monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture) -> None:
    monkeypatch.setenv("CRONWATCH_ENV", "development")
    cw, _ = client(cron_secret=None)
    with caplog.at_level(logging.WARNING, logger="cronwatch"):
        cw.routes(token=None)
        cw.job("a").handler(lambda ctx, request: "x")
    assert _warnings(caplog) == []


def test_handlers_opened_by_none_warn_once(production: None, caplog: pytest.LogCaptureFixture) -> None:
    cw, _ = client(cron_secret=None)
    ran: list[int] = []
    with caplog.at_level(logging.WARNING, logger="cronwatch"):
        handler = cw.job("a").handler(lambda ctx, request: ran.append(1))
        cw.job("b").handler(lambda ctx, request: None, secret=None)
    assert handler({"headers": {}}).status == 200 and ran == [1], "None still opts out"
    found = _warnings(caplog)
    assert len(found) == 1 and "cron_secret is None" in found[0] and "CRON_SECRET" in found[0]


def test_handlers_opened_by_name_do_not_warn(production: None, caplog: pytest.LogCaptureFixture) -> None:
    with caplog.at_level(logging.WARNING, logger="cronwatch"):
        cw, _ = client(cron_secret=cronwatch.OPEN)
        assert cw.cron_secret is None
        assert cw.job("a").handler(lambda ctx, request: None)({"headers": {}}).status == 200
        other, _ = client(cron_secret="s3cret")
        assert other.job("b").handler(lambda ctx, request: None, secret=cronwatch.OPEN)({"headers": {}}).status == 200
        assert other.job("c").handler(lambda ctx, request: None)({"headers": {}}).status == 401
        # A handler with a secret of its own is not open, whatever the client's is.
        closed, _ = client(cron_secret=None)
        assert closed.job("d").handler(lambda ctx, request: None, secret="mine")({"headers": {}}).status == 401
    assert _warnings(caplog) == []


def test_a_token_that_is_neither_text_nor_an_opt_out_still_raises() -> None:
    cw, _ = client()
    with pytest.raises(TypeError, match="routes: token must be a string"):
        cw.routes(token=False)


# ---------------------------------------------------------------- metrics


def test_a_metric_takes_any_real_number() -> None:
    cw, errors = client()
    job = cw.job("sales")
    with job.run() as ctx:
        ctx.metric("revenue", decimal.Decimal("12.50"))
        ctx.metric("share", fractions.Fraction(1, 4))
        ctx.metric("rows", 3)
        ctx.metric("big", decimal.Decimal(10) ** 30)
    run = cw.runs("sales")[0]
    assert run.status == "ok"
    assert run.metrics == {"revenue": 12.5, "share": 0.25, "rows": 3, "big": 1e30}
    assert all(type(value) in (int, float) for value in run.metrics.values())
    for bad in (True, "1", None, decimal.Decimal("NaN"), decimal.Decimal("Infinity"), float("inf"), 1j, decimal.Decimal("1e999")):
        with job.run() as ctx:
            with pytest.raises(ValueError, match="must be a finite number"):
                ctx.metric("x", bad)  # type: ignore[arg-type]
    handle = job.start()
    handle.metric("revenue", decimal.Decimal("1.5"))
    assert handle.finish().metrics == {"revenue": 1.5}
    cw.record_run({"id": "r1", "job": "sales", "status": "ok", "startedAt": 1, "finishedAt": 2, "metrics": {"revenue": decimal.Decimal("2.5")}})
    assert cw.get_run("r1").metrics == {"revenue": 2.5}
    assert errors == []


# ---------------------------------------------------------------- leaving a run


def test_a_block_left_in_another_context_still_ends_its_run() -> None:
    cw, errors = client()
    block = cw.job("a").run()
    contextvars.copy_context().run(block.__enter__)
    contextvars.copy_context().run(block.__exit__, None, None, None)
    assert [run.status for run in cw.runs("a")] == ["ok"]
    assert cronwatch.current() is None and errors == []


def test_an_error_that_cannot_be_printed_is_still_recorded() -> None:
    class Unprintable(Exception):
        def __str__(self) -> str:
            raise RuntimeError("no text")

    cw, errors = client()
    with pytest.raises(Unprintable):
        cw.job("a").run(lambda ctx: (_ for _ in ()).throw(Unprintable()))
    run = cw.runs("a")[0]
    assert run.status == "failed" and run.error.startswith("Unprintable: <exception str() failed>")
    assert errors == []


# ---------------------------------------------------------------- generators


def test_a_generator_function_is_refused_as_a_run() -> None:
    cw, _ = client()
    job = cw.job("a")

    def rows() -> Any:
        yield 1

    async def arows() -> Any:
        yield 1

    for target in (rows, arows):
        with pytest.raises(TypeError, match="generator function cannot be a run"):
            job.monitor(target)
    assert cw.runs("a") == [], "nothing was recorded as ok"
    # Handed back by a plain function, it is a failed run, as a coroutine is.
    for made in (rows, arows):
        with pytest.raises(TypeError, match="generator function cannot be a run"):
            job.run(lambda ctx, made=made: made())
    assert [run.status for run in cw.runs("a")] == ["failed", "failed"]

    async def later(ctx: Any) -> Any:
        return arows()

    with pytest.raises(TypeError, match="generator function cannot be a run"):
        asyncio.run(job.run(later))
    assert cw.runs("a")[0].status == "failed"


# ---------------------------------------------------------------- Postgres timeouts


def test_the_postgres_store_sets_only_the_timeouts_the_app_left_out(monkeypatch: pytest.MonkeyPatch) -> None:
    psycopg = pytest.importorskip("psycopg")
    from cronwatch import _pg

    monkeypatch.delenv("PGCONNECT_TIMEOUT", raising=False)
    monkeypatch.delenv("PGOPTIONS", raising=False)
    read = psycopg.conninfo.conninfo_to_dict
    made = read(_pg.with_timeouts(psycopg, "postgres://u:p@db.example/app"))
    assert made["host"] == "db.example" and made["password"] == "p"
    assert str(made["connect_timeout"]) == "10"
    assert [str(made[key]) for key in ("keepalives", "keepalives_idle", "keepalives_interval", "keepalives_count")] == ["1", "30", "10", "3"]
    if psycopg.pq.version() >= 120000:
        assert str(made["tcp_user_timeout"]) == "30000"
    assert "options" not in made

    own = read(_pg.with_timeouts(psycopg, "host=db connect_timeout=3 keepalives_idle=5 tcp_user_timeout=1000"))
    assert str(own["connect_timeout"]) == "3" and str(own["keepalives_idle"]) == "5" and str(own["tcp_user_timeout"]) == "1000"
    assert "keepalives_interval" not in own and "keepalives_count" not in own

    monkeypatch.setenv("PGCONNECT_TIMEOUT", "7")
    assert "connect_timeout" not in read(_pg.with_timeouts(psycopg, "host=db"))

    assert read(_pg.with_timeouts(psycopg, "host=db", 30_000))["options"] == "-c statement_timeout=30000"
    assert read(_pg.with_timeouts(psycopg, "host=db options='-c search_path=x'", 30_000))["options"] == "-c search_path=x -c statement_timeout=30000"
    assert read(_pg.with_timeouts(psycopg, "host=db options='-c statement_timeout=5'", 30_000))["options"] == "-c statement_timeout=5"
    assert _pg.with_timeouts(psycopg, "postgres://u:50%@db/app") == "postgres://u:50%@db/app", "an unreadable string is left for connect() to refuse"


def test_the_postgres_store_connects_with_them(monkeypatch: pytest.MonkeyPatch) -> None:
    psycopg = pytest.importorskip("psycopg")
    from cronwatch import _pg
    from cronwatch.stores.postgres import PostgresStore

    monkeypatch.delenv("PGCONNECT_TIMEOUT", raising=False)
    monkeypatch.delenv("PGOPTIONS", raising=False)
    seen: list[str] = []

    def connect(_psycopg: Any, conninfo: str, **_: Any) -> Any:
        seen.append(conninfo)
        raise psycopg.OperationalError("not here")

    monkeypatch.setattr(_pg, "connect", connect)
    with pytest.raises(psycopg.OperationalError):
        PostgresStore("host=db.example dbname=app", statement_timeout="30s").get_job("a")
    made = psycopg.conninfo.conninfo_to_dict(seen[0])
    assert str(made["connect_timeout"]) == "10" and made["options"] == "-c statement_timeout=30000"
    with pytest.raises(ValueError):
        PostgresStore("host=db", statement_timeout=0)


# ---------------------------------------------------------------- the SDK's client fixes


def test_a_secret_that_starts_in_one_flush_and_ends_in_the_next_is_redacted_whole() -> None:
    import json

    cw, _ = client()
    run = cw.job("deploy").start()
    run.log("starting")
    run.log("-----BEGIN PRIVATE KEY-----")
    run.log("MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7VJTUt9Us8cKj")
    run.flush()
    assert cw.get_run(run.id).output == "starting\n[redacted]"
    run.log("MFNRV0X0e1nSecondHalfOfTheKeySecondHalfOfTheKeySecondHalfOfTheKe")
    run.log("-----END PRIVATE KEY-----")
    run.log("api_token:")
    run.flush()
    assert cw.get_run(run.id).output == "starting\n[redacted]\napi_token:"
    # A label flushed, then its value: at the next flush, and again at finish.
    run.log("hunter2hunter2")
    run.flush()
    assert cw.get_run(run.id).output == "starting\n[redacted]\napi_token:\n[redacted]"
    run.log("password:")
    run.flush()
    run.log("correct-horse")
    finished = run.finish()
    assert finished.output == "starting\n[redacted]\napi_token:\n[redacted]\npassword:\n[redacted]"
    stored = json.dumps(cw.get_run(run.id).to_dict())
    assert not any(part in stored for part in ("SecondHalf", "hunter2", "correct-horse"))


def test_flushes_after_another_process_wrote_to_the_run_still_append_each_redacted() -> None:
    cw, _ = client()
    job = cw.job("deploy")
    run = job.start()
    run.log("one")
    run.flush()
    # Another handle on the same run (another process) appends in between.
    other = job.resume(run.id)
    other.log("from elsewhere")
    other.flush()
    run.log("token=abc123abc123")
    run.flush()
    assert cw.get_run(run.id).output == "one\nfrom elsewhere\ntoken=[redacted]"
    # A long run: the remembered tail stays bounded and the output capped.
    for i in range(6):
        run.log(f"line {i} {'x' * 6000}")
        run.flush()
        assert run._flushed is None or len(run._flushed[0]) <= 16 * 1024
    run.log("secret=tail-secret-value")
    finished = run.finish()
    assert len(finished.output) <= 16 * 1024 + 40
    assert finished.output.endswith("secret=[redacted]")
    assert "tail-secret-value" not in finished.output


def test_a_handle_kept_across_a_forget_declares_the_job_again_and_writes_it_once() -> None:
    from cronwatch.stores import MemoryStore

    upserts: list[str] = []

    class Counting(MemoryStore):
        def upsert_job(self, definition: Any, now: int) -> None:
            upserts.append(definition.name)
            super().upsert_job(definition, now)

    store = Counting()
    cw, _ = client(store=store)
    job = cw.job("nightly", schedule="0 2 * * *", description="Nightly export")
    job.run(lambda ctx: None)
    assert len(upserts) == 1
    cw.forget("nightly")
    assert cw.defined_jobs() == []

    job.run(lambda ctx: None)
    assert len(upserts) == 2
    assert [d.name for d in cw.defined_jobs()] == ["nightly"], "declared again, so checks and sources see it"
    job.run(lambda ctx: None)
    job.run(lambda ctx: None)
    assert len(upserts) == 2, "written once, not on every run"
    assert cw.job_summary("nightly").definition.description == "Nightly export"

    # Forgotten by another process since: this one's check writes it again.
    store.delete_job("nightly")
    cw.check()
    assert store.get_job("nightly") is not None


def test_an_on_error_that_raises_does_not_stop_a_check_the_jobs_after_it_or_a_dashboard_read() -> None:
    from cronwatch.stores import MemoryStore

    broken: set[str] = set()

    class Flaky(MemoryStore):
        def list_runs(self, job: str, limit: int) -> Any:
            if job in broken:
                raise ConnectionError("store down")
            return super().list_runs(job, limit)

        def prune(self, before: int) -> int:
            raise ConnectionError("prune down")

    class BadSource:
        name = "bad"

        def sync(self, host: Any) -> None:
            raise RuntimeError("source down")

    calls: list[str] = []
    sent: list[Any] = []
    clock = [1_700_000_000_000]

    def on_error(error: BaseException, where: str) -> None:
        calls.append(where)
        raise RuntimeError("on_error raised")

    cw = cronwatch.Cronwatch(store=Flaky(), sources=[BadSource()], alerts=[cronwatch.Custom("list", sent.append)], on_error=on_error, now=lambda: clock[0])
    cw.job("a-broken", schedule="every 5m")
    cw.job("b-late", schedule="every 5m")
    cw.jobs()  # written to the store now, so both are due an hour on
    broken.add("a-broken")
    clock[0] += 3_600_000
    result = cw.check()
    assert len(calls) >= 3, "the source, the broken job, and the prune were each reported"
    assert [job.name for job in result.jobs] == ["a-broken", "b-late"]
    assert [f"{alert.job} {alert.type}" for alert in sent] == ["b-late missed"], "the job after the broken one was still checked"
    assert cw.job_summary("a-broken").health == "failing"


@pytest.mark.parametrize("read", ["check", "jobs", "job_summary"])
def test_a_forget_that_lands_while_a_read_is_writing_declarations_is_not_undone(read: str) -> None:
    from cronwatch.stores import MemoryStore

    forget: list[Any] = []

    class Interleaved(MemoryStore):
        # Another thread's forget (a dashboard delete), landing as the read's loop writes the job before it.
        def upsert_job(self, definition: Any, now: int) -> None:
            super().upsert_job(definition, now)
            if definition.name == "keep" and forget:
                forget.pop()()

    store = Interleaved()
    cw, errors = client(store=store)
    cw.job("keep", schedule="0 2 * * *")
    cw.job("nightly", schedule="0 2 * * *")
    # Neither is written yet, so the read's own loop is what would write them.
    if read == "job_summary":
        # Here the forget lands between the read of the declaration and its write.
        ready = cw._ensure_ready
        calls = [0]

        def ensure_ready() -> None:
            calls[0] += 1
            if calls[0] == 2:
                cw.forget("nightly")
            ready()

        cw._ensure_ready = ensure_ready  # type: ignore[method-assign]
        assert cw.job_summary("nightly") is None
        cw._ensure_ready = ready  # type: ignore[method-assign]
    else:
        forget.append(lambda: cw.forget("nightly"))
        getattr(cw, read)()
    assert [d.name for d in cw.defined_jobs()] == ["keep"], "still forgotten here"
    assert store.get_job("nightly") is None, "not written again"
    assert store.get_state("nightly") is None, "no state left behind"
    # Its next run still brings it back.
    cw.run("nightly", lambda ctx: None)
    assert store.get_job("nightly") is not None
    assert errors == []
