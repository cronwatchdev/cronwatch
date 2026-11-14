"""The client's behaviour, ported from the SDK's test/client.test.ts. The
fetch-style handler() tests belong to the web phase."""

from __future__ import annotations

from collections.abc import Callable

import pytest

import cronwatch
from cronwatch import _evaluate
from cronwatch._js import date_utc
from cronwatch.types import JobDefinition, Run

from helpers import HOUR, MIN, T0, Errors, boom, make


def test_run_records_output_metrics_and_duration_and_returns_the_result() -> None:
    cw, c, _ = make()
    job = cw.job("report", schedule="0 2 * * *")

    def work(ctx: cronwatch.JobContext) -> str:
        ctx.log("hello", {"n": 1})
        ctx.metric("rows", 42)
        c.advance(1500)
        return "done"

    assert job.run(work) == "done"
    [run] = cw.runs("report")
    assert run.status == "ok"
    assert run.duration_ms == 1500
    assert run.output == 'hello {"n":1}'
    assert run.metrics == {"rows": 42}
    summary = cw.job_summary("report")
    assert summary is not None
    assert summary.health == "healthy"
    assert summary.next_expected_at == date_utc(2026, 0, 6, 2, 0)


def test_the_block_and_the_decorator_record_runs_too() -> None:
    cw, c, alerts = make()
    job = cw.job("report")
    with job.run() as ctx:
        ctx.log("from a block")
        c.advance(250)
    [run] = cw.runs("report")
    assert (run.status, run.output, run.duration_ms) == ("ok", "from a block", 250)

    @job.monitor
    def nightly(rows: int) -> int:
        context = cronwatch.current()
        assert context is not None
        context.log(f"wrote {rows} rows")
        context.metric("rows", rows)
        return rows * 2

    assert nightly(21) == 42
    assert cronwatch.current() is None
    latest = cw.runs("report")[0]
    assert (latest.output, latest.metrics) == ("wrote 21 rows", {"rows": 21})

    with pytest.raises(RuntimeError, match="in the block"):
        with job.run() as ctx:
            raise RuntimeError("in the block")
    assert cw.runs("report")[0].status == "failed"
    assert cw.runs("report")[0].error.startswith("RuntimeError: in the block\n    at ")
    assert alerts.types() == ["failed"]

    @job.monitor(trigger="celery")
    def task() -> None:
        pass

    task()
    assert cw.runs("report")[0].trigger == "celery"
    assert alerts.types() == ["failed", "recovered"]


def test_a_throwing_job_is_recorded_as_failed_alerts_and_rethrows() -> None:
    cw, _, alerts = make()
    job = cw.job("nightly")
    with pytest.raises(RuntimeError, match="db down"):
        job.run(boom("db down"))
    [run] = cw.runs("nightly")
    assert run.status == "failed"
    assert "RuntimeError: db down" in run.error
    assert alerts.types() == ["failed"]
    assert "db down" in alerts.alerts[0].message
    assert cw.job_summary("nightly").health == "failing"


def test_expect_turns_a_quiet_success_into_a_failure() -> None:
    cw, c, alerts = make()
    job = cw.job("export", expect="wrote")
    job.run(lambda ctx: ctx.log("wrote 12 files"))
    assert alerts.types() == []
    c.advance(HOUR)
    job.run(lambda ctx: ctx.log("nothing to do"))
    [run, _] = cw.runs("export")
    assert run.status == "failed"
    assert 'did not contain "wrote"' in run.error
    assert alerts.types() == ["failed"]
    # A returned string counts as output too.
    job.run(lambda ctx: "wrote 3 files")
    assert alerts.types() == ["failed", "recovered"]


def test_run_defines_on_first_use_and_validates_names_and_schedules() -> None:
    cw, _, _ = make()
    assert cw.run("adhoc", lambda ctx: 1, schedule="every 5m") == 1
    assert len(cw.jobs()) == 1
    with pytest.raises(ValueError, match="job name"):
        cw.job("bad name!")
    with pytest.raises(ValueError, match="not a cron expression"):
        cw.job("x", schedule="nope")
    with pytest.raises(ValueError, match="grace"):
        cw.job("x", grace="soon")
    with pytest.raises(TypeError, match="unknown option"):
        cw.job("x", schedul="0 2 * * *")


def test_check_finds_a_missed_run_once_and_a_later_run_recovers() -> None:
    cw, c, alerts = make()
    job = cw.job("sync", schedule="every 1h", grace="10m")
    cw.check()  # registers at T0
    c.advance(30 * MIN)
    assert cw.check().alerts == []
    c.set(T0 + 70 * MIN + 1)
    result = cw.check()
    assert [a.type for a in result.alerts] == ["missed"]
    assert result.jobs[0].health == "late"
    assert cw.check().alerts == [], "no repeat"
    job.run(lambda ctx: None)
    assert alerts.types() == ["missed", "recovered"]
    assert cw.job_summary("sync").health == "healthy"


def test_a_job_declared_again_without_its_schedule_closes_missed_with_a_recovery_once() -> None:
    cw, c, alerts = make()
    cw.job("sync", schedule="every 1h", grace="10m")
    cw.check()
    c.set(T0 + 70 * MIN + 1)
    assert [a.type for a in cw.check().alerts] == ["missed"]
    job = cw.job("sync")
    c.advance(MIN)
    result = cw.check()
    assert [a.type for a in result.alerts] == ["recovered"]
    alert = result.alerts[0]
    assert alert.title == "sync is no longer scheduled"
    assert alert.message == "Missed since 2026-01-05 10:40:00 UTC (1m ago). It has no schedule now, so nothing is due; the missed alert is closed."
    assert alert.to_dict()["details"] == {"after": ["missed"], "reason": "unscheduled", "since": T0 + 70 * MIN + 1}
    assert result.jobs[0].health == "never_ran"
    assert cw.check().alerts == [], "no repeat"
    job.run(lambda ctx: None)
    assert alerts.types() == ["missed", "recovered"], "the next run owes nothing"


def test_a_schedule_removed_while_silenced_closes_missed_with_its_recovery_which_the_silence_does_not_hold_back() -> None:
    cw, c, alerts = make()
    cw.job("sync", schedule="every 1h", grace="10m")
    cw.check()
    c.set(T0 + 70 * MIN + 1)
    cw.check()
    cw.silence("sync", "1h")
    cw.job("sync")
    c.advance(MIN)
    assert [str(a.type) for a in cw.check().alerts] == ["recovered"]
    assert cw.job_summary("sync").open == []
    c.advance(2 * HOUR)
    assert cw.check().alerts == []
    assert alerts.types() == ["missed", "recovered"]


def test_check_marks_a_run_that_never_finished_as_stuck() -> None:
    cw, c, alerts = make()
    job = cw.job("long", timeout="5m")
    block = job.run()
    block.__enter__()  # a run that never comes back
    assert cw.runs("long")[0].status == "running"
    c.advance(4 * MIN)
    assert cw.check().alerts == []
    c.advance(2 * MIN)
    result = cw.check()
    assert [a.type for a in result.alerts] == ["stuck"]
    assert cw.runs("long")[0].status == "timeout"
    assert result.jobs[0].health == "stuck"
    assert "never reported finishing" in alerts.alerts[0].message
    c.advance(MIN)
    block.__exit__(None, None, None)  # it came back after all: a late success recovers
    assert alerts.types() == ["stuck", "recovered"]
    assert cronwatch.current() is None


def test_slow_and_over_budget_alerts_come_from_the_jobs_own_baseline() -> None:
    cw, c, alerts = make()
    job = cw.job("agent", budget={"cost": 1})

    def usual(ctx: cronwatch.JobContext) -> None:
        c.advance(1000)
        ctx.metrics({"tokens": 1000, "cost": 0.5})

    for _ in range(5):
        job.run(usual)
        c.advance(HOUR)
    assert alerts.types() == []

    def slow(ctx: cronwatch.JobContext) -> None:
        c.advance(15_000)
        ctx.metrics(tokens=1000, cost=0.5)

    job.run(slow)
    assert alerts.types() == ["slow"]
    c.advance(HOUR)

    def costly(ctx: cronwatch.JobContext) -> None:
        c.advance(1000)
        ctx.metrics({"tokens": 5000, "cost": 1.2})

    job.run(costly)
    assert alerts.types() == ["slow", "over_budget"]
    last = alerts.alerts[1]
    assert "cost: 1.2, limit 1 (budget)" in last.message
    assert "tokens: 5,000, limit 3,000 (three times the usual 1,000)" in last.message
    c.advance(HOUR)
    job.run(usual)
    assert alerts.types() == ["slow", "over_budget", "recovered"]


def test_an_under_floor_alert_names_the_metric_and_what_it_was_judged_against() -> None:
    cw, c, alerts = make()
    job = cw.job("import", floor={"files": 1})

    def reporting(rows: int, files: int) -> Callable[[cronwatch.JobContext], None]:
        def work(ctx: cronwatch.JobContext) -> None:
            c.advance(1000)
            ctx.metrics(rows=rows, files=files)

        return work

    for i in range(5):
        job.run(reporting(4812 + i, 2))
        c.advance(HOUR)
    job.run(reporting(0, 0))
    assert alerts.types() == ["under_floor"]
    alert = alerts.alerts[0]
    assert alert.title == "import fell short"
    assert "rows: 0 (the last 5 runs all reported more than 0, the lowest 4,812)" in alert.message
    assert "files: 0, below the floor of 1." in alert.message
    c.advance(HOUR)
    job.run(reporting(0, 0))
    assert alerts.types() == ["under_floor"]
    c.advance(HOUR)
    job.run(reporting(10, 1))
    assert alerts.types() == ["under_floor", "recovered"]


def test_a_floor_must_be_a_finite_number_and_no_higher_than_its_ceiling() -> None:
    cw, _, _ = make()
    with pytest.raises(ValueError, match=r"floor\.rows must be a finite number \(got NaN\)"):
        cw.job("a", floor={"rows": float("nan")})
    with pytest.raises(ValueError, match=r"floor must be an object"):
        cw.job("a", floor=[1])
    with pytest.raises(ValueError, match=r"floor\.cost \(3\) is above budget\.cost \(2\), so every run would alert"):
        cw.job("b", floor={"cost": 3}, budget={"cost": 2})
    cw.job("c", floor={"delta": -5}, budget={"delta": 5})


def _ok(at: int, metrics: dict[str, float]) -> Run:
    return Run(id=f"r{at}", job="j", status="ok", started_at=at, finished_at=at + 1000, duration_ms=1000, metrics=metrics)


def test_floors_a_floor_or_0_after_five_runs_that_all_reported_more() -> None:
    def finish(definition: JobDefinition, run: Run, state: cronwatch.JobState, history: list[Run], now: int) -> _evaluate.Evaluation:
        return _evaluate.on_run_finish(definition, run, state, history, now)

    floored = JobDefinition({"name": "j", "floor": {"rows": 10}})
    short = finish(floored, _ok(T0, {"rows": 9}), _evaluate.empty_state("j"), [], T0 + 1000)
    assert [a.type for a in short.alerts] == ["under_floor"]
    assert short.alerts[0].details["breaches"] == [{"metric": "rows", "value": 9, "limit": 10, "basis": "floor"}]
    back = finish(floored, _ok(T0 + HOUR, {"rows": 10}), short.state, [], T0 + HOUR + 1000)
    assert [a.type for a in back.alerts] == ["recovered"]
    assert back.state.under_floor is None

    bare = JobDefinition({"name": "j"})
    history = [_ok(T0 - i * HOUR, {"rows": 100 * i, "errors": 0}) for i in range(1, 6)]
    assert finish(bare, _ok(T0, {"rows": 0}), _evaluate.empty_state("j"), history[1:], T0).alerts == [], "four runs are not a baseline"
    assert finish(bare, _ok(T0, {"rows": 1, "errors": 0}), _evaluate.empty_state("j"), history, T0).alerts == [], "an always-0 metric never alerts"
    zero = finish(bare, _ok(T0, {"rows": 0, "errors": 0}), _evaluate.empty_state("j"), history, T0)
    assert zero.alerts[0].details["breaches"] == [
        {"metric": "rows", "value": 0, "limit": 100, "basis": "the last 5 runs all reported more than 0, the lowest 100"}
    ]
    assert zero.state.under_floor == ["rows"]

    # A job that keeps writing nothing stays open, past the point where its zeros are all the history there is.
    state = zero.state
    runs = list(history)
    for i in range(1, 31):
        runs.insert(0, _ok(T0 + (i - 1) * HOUR, {"rows": 0, "errors": 0}))
        following = finish(bare, _ok(T0 + i * HOUR, {"rows": 0, "errors": 0}), state, runs[:25], T0 + i * HOUR)
        assert following.alerts == []
        assert following.state.open["under_floor"] == T0
        state = following.state
    recovered = finish(bare, _ok(T0 + 31 * HOUR, {"rows": 5, "errors": 0}), state, runs[:25], T0 + 31 * HOUR)
    assert [a.type for a in recovered.alerts] == ["recovered"]
    assert recovered.alerts[0].details["after"] == ["under_floor"]

    # A metric that has reported 0 before is judged as usual for it, and a floor of 0 turns the check off.
    mixed = [*history[:4], _ok(T0 - 6 * HOUR, {"rows": 0})]
    assert finish(bare, _ok(T0, {"rows": 0}), _evaluate.empty_state("j"), mixed, T0).alerts == []
    assert finish(JobDefinition({"name": "j", "floor": {"rows": 0}}), _ok(T0, {"rows": 0}), _evaluate.empty_state("j"), history, T0).alerts == []


def test_silence_swallows_alerts_and_nothing_opens_underneath_unsilence_alerts_again() -> None:
    cw, c, alerts = make()
    job = cw.job("flaky")
    cw.silence("flaky", "1h")
    with pytest.raises(RuntimeError):
        job.run(boom("x"))
    assert alerts.types() == []
    assert cw.job_summary("flaky").health == "silenced"
    cw.unsilence("flaky")
    with pytest.raises(RuntimeError):
        job.run(boom("y"))
    assert alerts.types() == ["failed"]


def test_a_silence_ends_on_a_whole_millisecond_never_past_2_to_the_53_minus_1() -> None:
    cw, c, _ = make()
    cw.job("long")
    top = 2**53 - 1
    assert cw.silence("long", "99999999999999999999w").silenced_until == top
    assert cw.silence("long", 1e300).silenced_until == top
    assert cw.silence("long", 1.5).silenced_until == c.now() + 1
    from helpers import send

    web = cw.routes(token=None, base_path="/cronwatch")
    res = send(web, "POST", "/cronwatch/api/jobs/long/silence", {"content-type": "application/json"}, '{"for":"99999999999999999999w"}')
    assert res.status == 200
    assert res.json()["job"]["silencedUntil"] == top
    assert cw.store.get_state("long").silenced_until == top


def test_triage_output_is_attached_to_failure_alerts_and_never_blocks_them() -> None:
    cw, _, alerts = make(triage=lambda ctx: f"Probably {ctx.alert.job}'s database.")
    with pytest.raises(RuntimeError):
        cw.run("t", boom())
    assert alerts.alerts[0].triage == "Probably t's database."

    errors = Errors()

    def broken(ctx: cronwatch.TriageContext) -> str:
        raise RuntimeError("api down")

    cw2, _, alerts2 = make(triage=broken, on_error=errors)
    with pytest.raises(RuntimeError):
        cw2.run("t", boom())
    assert alerts2.types() == ["failed"]
    assert alerts2.alerts[0].triage is None
    assert alerts2.alerts[0].to_dict()["triage"] is None, "tried, and gave nothing"
    assert errors.wheres == ["triage for t"]


def test_forget_removes_the_job_and_its_runs() -> None:
    cw, _, _ = make()
    cw.run("gone", lambda ctx: None)
    assert len(cw.jobs()) == 1
    cw.forget("gone")
    assert len(cw.jobs()) == 0
    assert cw.job_summary("gone") is None


def test_after_a_forget_run_and_resume_run_by_name_declare_the_job_again_as_it_was_written() -> None:
    cw, c, alerts = make()
    cw.job("nightly", schedule="0 2 * * *", description="Nightly export")
    cw.run("nightly", lambda ctx: None)
    handle = cw.job("nightly", schedule="0 2 * * *", description="Nightly export").start()
    cw.forget("nightly")
    assert cw.job_summary("nightly") is None
    cw.run("nightly", lambda ctx: None)
    summary = cw.job_summary("nightly")
    assert summary.definition.schedule == "0 2 * * *"
    assert summary.definition.description == "Nightly export"
    cw.forget("nightly")
    assert cw.resume_run("nightly", handle.id).id == handle.id
    c.advance(HOUR)
    cw.check()
    assert cw.job_summary("nightly").definition.schedule == "0 2 * * *"
    assert "recovered" not in alerts.types(), "no 'no longer scheduled' recovery"


def test_a_recovery_for_an_alert_sent_before_the_silence_is_sent_during_it_new_problems_are_not() -> None:
    cw, _, alerts = make()
    job = cw.job("flaky")
    with pytest.raises(RuntimeError):
        job.run(boom("x"))
    cw.silence("flaky", "1h")
    job.run(lambda ctx: "ok")
    assert alerts.types() == ["failed", "recovered"]
    with pytest.raises(RuntimeError):
        job.run(boom("y"))
    assert alerts.types() == ["failed", "recovered"]


def test_a_failing_alert_channel_does_not_break_the_run() -> None:
    errors = Errors()

    class Broken:
        name = "broken"

        def send(self, alert: cronwatch.Alert) -> None:
            raise ConnectionError("no network")

    cw, _, _ = make(alerts=[Broken()], on_error=errors)
    with pytest.raises(RuntimeError, match="job"):
        cw.run("x", boom("job"))
    assert errors.wheres == ["alert channel broken"]


def test_plain_functions_and_custom_channels_take_alerts() -> None:
    seen: list[str] = []

    def pager(alert: cronwatch.Alert) -> None:
        seen.append(f"pager {alert.title}")

    reported: list[str] = []

    def with_context(alert: cronwatch.Alert, context: cronwatch.ChannelContext) -> None:
        context.on_error(RuntimeError("one recipient refused it"))
        seen.append("context")

    errors = Errors()
    cw, _, _ = make(alerts=[pager, cronwatch.Custom("custom", with_context)], on_error=errors)
    with pytest.raises(RuntimeError):
        cw.run("p", boom())
    assert sorted(seen) == ["context", "pager p failed"]
    assert errors.wheres == ["alert channel custom"]
    assert reported == []


def test_configure_makes_the_process_client() -> None:
    made = cronwatch.configure(cron_secret=None, alerts=[])
    assert cronwatch.client() is made
    again = cronwatch.configure(cron_secret=None, alerts=[])
    assert cronwatch.client() is again


def test_a_plain_function_handing_back_a_coroutine_is_a_failed_run_not_a_quick_success() -> None:
    cw, _, alerts = make()
    job = cw.job("a")
    ran: list[int] = []

    async def work(ctx: object) -> None:
        ran.append(1)

    with pytest.raises(TypeError, match="returned a coroutine"):
        job.run(lambda ctx: work(ctx))
    [run] = cw.runs("a")
    assert run.status == "failed"
    assert run.error.startswith("TypeError: job a: the function returned a coroutine")
    assert ran == [], "never started, and closed rather than left to warn"
    assert alerts.types() == ["failed"]


def test_run_and_monitor_are_typed_for_a_type_checker() -> None:
    """py.typed is shipped, so a decorated function must keep its signature
    rather than become Any (mypy and pyright read these overloads)."""
    import typing

    from cronwatch._client import JobHandle

    assert len(typing.get_overloads(JobHandle.monitor)) == 2
    assert len(typing.get_overloads(JobHandle.run)) == 3
    assert JobHandle.handler.__annotations__["return"] == "Handler"


def test_record_run_refuses_fields_evaluation_cannot_read() -> None:
    cw, _, _ = make(cron_secret=None)
    cw.job("imported")
    run = {"id": "r", "job": "imported", "status": "ok", "startedAt": 1, "finishedAt": 2, "durationMs": 1, "error": None, "output": None, "metrics": {}, "trigger": "source"}
    for patch, message in [
        ({"status": "succeeded"}, "record_run: status must be"),
        ({"startedAt": float("nan")}, "record_run: started_at must be a finite number"),
        ({"finishedAt": "2"}, "record_run: finished_at must be a finite number or None"),
        ({"durationMs": float("inf")}, "record_run: duration_ms must be a finite number or None"),
        ({"error": 5}, "record_run: error must be a string or None"),
        ({"output": b"bytes"}, "record_run: output must be a string or None"),
        ({"trigger": None}, "record_run: trigger must be a string"),
    ]:
        with pytest.raises(ValueError, match=message):
            cw.record_run({**run, **patch})
    assert cw.store.get_run("r") is None


def test_stuck_stays_open_through_later_starts_failures_and_timeouts_and_closes_with_a_recovery_on_the_next_success() -> None:
    d = JobDefinition({"name": "j"})

    def run(status: str, started_at: int) -> Run:
        return Run(id=f"r{started_at}", job="j", status=status, started_at=started_at, finished_at=None if status == "running" else started_at + 1000, duration_ms=None if status == "running" else 1000)

    def start_now(state: Any, at: int) -> Any:
        return _evaluate.on_run_start(d, run("running", at), state, at)

    stuck = _evaluate.on_run_finish(d, run("timeout", T0 - HOUR), _evaluate.empty_state("j"), [], T0)
    assert [a.type for a in stuck.alerts] == ["stuck"]
    # The next run starting closes nothing: it may hang as the last one did.
    started = start_now(stuck.state, T0 + MIN)
    assert started.open["stuck"] == T0
    assert not started.pending_recovery
    # It times out too: stuck is already open, so there is no second alert.
    again = _evaluate.on_run_finish(d, run("timeout", T0 + MIN), started, [], T0 + HOUR)
    assert again.alerts == []
    assert again.state.open["stuck"] == T0, "open since the first time"
    assert again.state.consecutive_failures == 2
    # A run that fails opens failed by its own rule and leaves stuck open.
    failed = _evaluate.on_run_finish(d, run("failed", T0 + HOUR), start_now(again.state, T0 + HOUR), [], T0 + HOUR + 1000)
    assert [a.type for a in failed.alerts] == ["failed"]
    assert failed.state.open["stuck"] == T0
    # The first success closes both, with one recovery naming both.
    ok = _evaluate.on_run_finish(d, run("ok", T0 + 2 * HOUR), start_now(failed.state, T0 + 2 * HOUR), [], T0 + 2 * HOUR + 1000)
    assert [a.type for a in ok.alerts] == ["recovered"]
    assert sorted(ok.alerts[0].details["after"]) == ["failed", "stuck"]
    assert not ok.state.open and not ok.state.pending_recovery
    # Stuck never closes on a start, whether or not the run catches the schedule up.
    hourly = JobDefinition({"name": "j", "schedule": "0 * * * *", "timezone": "UTC", "grace": "10m"})
    at = lambda h, m=0: date_utc(2026, 0, 5, h, m)  # noqa: E731
    covering = Run(id="c", job="j", status="ok", started_at=at(3, 2), finished_at=at(3, 30), duration_ms=28 * MIN)
    both = _evaluate.normalize_state({"job": "j", "open": {"stuck": at(3), "missed": at(3, 11)}}, "j")
    assert dict(_evaluate.on_run_start(hourly, covering, both, at(3, 30)).open) == {"stuck": at(3)}


def test_a_job_that_hangs_on_every_run_alerts_stuck_once_and_recovers_on_its_first_success() -> None:
    cw, c, alerts = make()
    job = cw.job("hangs", schedule="*/5 * * * *", timezone="UTC", timeout="10m")
    # Twelve runs, five minutes apart, none of which ever finishes.
    handles = []
    for _ in range(12):
        handles.append(job.start())
        c.advance(5 * MIN)
        cw.check()
    assert alerts.types() == ["stuck"]
    assert cw.job_summary("hangs").health == "stuck"
    # A run that fails alerts as failed by its own rule; stuck stays open beside it.
    with pytest.raises(RuntimeError):
        job.run(boom("boom"))
    assert alerts.types() == ["stuck", "failed"]
    assert sorted(cw.job_summary("hangs").open) == ["failed", "stuck"]
    # A hung run that reports failing late is not counted again and alerts nothing.
    handles[0].fail(RuntimeError("late"))
    assert alerts.types() == ["stuck", "failed"]
    # The first success closes both with one recovery.
    job.run(lambda ctx: None)
    assert alerts.types() == ["stuck", "failed", "recovered"]
    assert sorted(alerts.alerts[2].details["after"]) == ["failed", "stuck"]
    assert cw.job_summary("hangs").open == []


def test_a_stuck_run_that_finishes_ok_late_closes_stuck_with_a_recovery_though_other_runs_started_meanwhile() -> None:
    cw, c, alerts = make()
    job = cw.job("slow-queue", timeout="10m")
    first = job.start()
    c.advance(11 * MIN)
    cw.check()
    assert alerts.types() == ["stuck"]
    second = job.start()
    assert cw.job_summary("slow-queue").open == ["stuck"], "a start leaves stuck open"
    first.finish()
    assert alerts.types() == ["stuck", "recovered"]
    second.finish()
    assert alerts.types() == ["stuck", "recovered"]


# ---------------------------------------------------------------- the start deadline


class _Hanging:
    """A store whose first insert_run (a run's start) hangs until released;
    with `down`, every later insert finds the store down, not hanging."""

    def __init__(self, down: bool = False) -> None:
        import threading

        from cronwatch.stores import MemoryStore

        self.base = MemoryStore()
        self.release = threading.Event()
        self.down = down
        self.inserts = 0
        self._count = threading.Lock()

    def __getattr__(self, name: str) -> Any:
        return getattr(self.base, name)

    def insert_run(self, run: Run) -> None:
        with self._count:
            self.inserts += 1
            first = self.inserts == 1
        if first:
            assert self.release.wait(10), "never released"
        elif self.down:
            raise RuntimeError("store down")
        self.base.insert_run(run)


def _until(condition: Callable[[], bool], what: str) -> None:
    import time

    deadline = time.monotonic() + 5
    while not condition():
        assert time.monotonic() < deadline, what
        time.sleep(0.005)


def _hung(down: bool = False) -> tuple[Any, cronwatch.Cronwatch, Any, list[str]]:
    store = _Hanging(down)
    errors: list[str] = []
    cw, _, alerts = make(store=store, on_error=lambda error, where: errors.append(f"{where}: {error}"))
    cw._start_deadline_ms = 150
    return store, cw, alerts, errors


def test_a_store_that_hangs_while_recording_a_runs_start_does_not_hold_the_job_back_and_leaves_no_running_row() -> None:
    import time

    assert cronwatch._client.START_DEADLINE_MS == 5_000
    store, cw, alerts, errors = _hung()
    job = cw.job("nightly")
    began = time.monotonic()
    waited: list[float] = []

    def work(ctx: Any) -> str:
        waited.append(time.monotonic() - began)
        ctx.log("did the work")
        # The job is done; the late start lands first, and the finish is written over it.
        store.release.set()
        _until(lambda: len(store.base.running_runs()) == 1, "the start write never landed")
        return "done"

    assert job.run(work) == "done"
    assert 0.14 <= waited[0] < 2, "the job ran once the deadline passed, not before"
    assert errors == ["recording nightly: the store took longer than 150ms to record the start of nightly; running it unrecorded"]
    runs = cw.runs("nightly")
    assert [(run.status, run.output) for run in runs] == [("ok", "did the work")]
    assert store.base.running_runs() == []
    assert alerts.types() == []


def test_a_start_write_that_lands_after_the_finish_was_recorded_is_refused_and_changes_nothing() -> None:
    store, cw, alerts, errors = _hung()
    assert cw.job("nightly").run(lambda ctx: "done") == "done"
    # The finish inserted the run, finished, while the start write was still hung.
    assert [run.status for run in cw.runs("nightly")] == ["ok"]
    store.release.set()
    _until(lambda: not cw._abandoned["start"].is_alive(), "the start write never ended")
    assert [run.status for run in cw.runs("nightly")] == ["ok"]
    assert store.base.running_runs() == []
    assert len(errors) == 1 and alerts.types() == []


def test_a_start_write_that_lands_after_the_finish_failed_is_finished_then_not_left_running() -> None:
    store, cw, alerts, errors = _hung(down=True)
    with pytest.raises(RuntimeError, match="boom"):
        cw.job("nightly").run(boom("boom"))
    assert [e.split(":")[0] for e in errors] == ["recording nightly", "recording nightly"]
    assert cw.runs("nightly") == []
    # The start write lands at last, after the finish gave up: the run is finished and judged now.
    store.release.set()
    _until(lambda: alerts.types() == ["failed"], "the late start was never finished")
    runs = cw.runs("nightly")
    assert len(runs) == 1 and runs[0].status == "failed" and "boom" in runs[0].error
    assert store.base.running_runs() == []


def test_while_a_start_write_is_hung_later_runs_do_not_wait_behind_it() -> None:
    import time

    store, cw, alerts, errors = _hung()
    job = cw.job("nightly")
    job.run(lambda ctx: None)
    began = time.monotonic()
    job.run(lambda ctx: None)
    assert time.monotonic() - began < 0.1, "ran at once: one thread waits on the store, not one per run"
    assert errors[1] == "recording nightly: the store has not answered an earlier start write yet; running nightly unrecorded"
    assert [run.status for run in cw.runs("nightly")] == ["ok", "ok"], "each was recorded at its finish"
    store.release.set()
    _until(lambda: not cw._abandoned["start"].is_alive(), "the start write never ended")
    job.run(lambda ctx: None)
    assert len(errors) == 2, "and once the store answers, starts are written again"
    assert store.base.running_runs() == []


def test_an_async_run_and_a_handler_are_not_held_back_by_a_hung_store_either() -> None:
    import asyncio

    store, cw, _, errors = _hung()

    async def work(ctx: Any) -> str:
        return "done"

    assert asyncio.run(cw.job("nightly").run(work)) == "done"
    assert errors == ["recording nightly: the store took longer than 150ms to record the start of nightly; running it unrecorded"]
    store.release.set()
    _until(lambda: not cw._abandoned["start"].is_alive(), "the start write never ended")

    store, cw, _, errors = _hung()
    answer = cw.job("hook").handler(lambda ctx, request: "ran")({"headers": {}})
    assert answer.status == 200 and len(errors) == 1
    store.release.set()
    _until(lambda: not cw._abandoned["start"].is_alive(), "the start write never ended")
    assert store.base.running_runs() == []


def test_a_store_that_fails_at_the_start_is_reported_as_before_and_the_run_recorded_at_its_finish() -> None:
    from helpers import Flaky

    from cronwatch.stores import MemoryStore

    broken = {"upsert_job"}
    errors = Errors()
    cw, _, _ = make(store=Flaky(MemoryStore(), broken), on_error=errors)

    def work(ctx: Any) -> int:
        broken.clear()
        return 1

    assert cw.job("nightly").run(work) == 1
    assert errors.wheres == ["recording nightly"] and errors.messages == ["store down: upsert_job"]
    assert [run.status for run in cw.runs("nightly")] == ["ok"]


def test_a_store_only_its_own_thread_may_use_has_its_start_written_in_the_callers_thread(tmp_path: Any) -> None:
    import sqlite3

    from cronwatch.stores import SqliteStore

    # sqlite3's default: only the thread that opened the connection may use it.
    connection = sqlite3.connect(tmp_path / "own.db", isolation_level=None)
    errors = Errors()
    cw, _, _ = make(store=SqliteStore(connection=connection), on_error=errors)
    seen: list[str] = []
    cw.job("nightly").run(lambda ctx: seen.append(cw.runs("nightly")[0].status))
    assert seen == ["running"], "the start was recorded before the job ran"
    assert [run.status for run in cw.runs("nightly")] == ["ok"]
    assert errors.items == []


def test_a_start_that_lands_while_a_failing_finish_is_mid_write_is_finished_by_that_finish() -> None:
    import threading

    from cronwatch.stores import MemoryStore

    base = MemoryStore()
    release = threading.Event()
    landed = threading.Event()
    inserts = [0]
    reads_down = [False]

    def insert_run(run: Run) -> None:
        inserts[0] += 1
        if inserts[0] == 1:
            assert release.wait(10), "never released"
            base.insert_run(run)
            landed.set()
            return
        # The finish, written as a run of its own: the start lands while it is
        # under way, and then both its insert and its read-back fail.
        release.set()
        assert landed.wait(10), "the start write never landed"
        reads_down[0] = True
        raise RuntimeError("store down")

    def get_run(run_id: str) -> Any:
        if reads_down[0]:
            reads_down[0] = False
            raise RuntimeError("store down")
        return base.get_run(run_id)

    from helpers import Wrapped

    errors: list[str] = []
    cw, _, alerts = make(store=Wrapped(base, insert_run=insert_run, get_run=get_run), on_error=lambda error, where: errors.append(f"{where}: {error}"))
    cw._start_deadline_ms = 150
    with pytest.raises(RuntimeError, match="boom"):
        cw.job("nightly").run(boom("boom"))
    # Nothing is left to another thread: by the time run() is back, the row the start left is finished.
    assert base.running_runs() == []
    runs = cw.runs("nightly")
    assert len(runs) == 1 and runs[0].status == "failed" and "boom" in runs[0].error
    assert alerts.types() == ["failed"]
    assert [e.split(":")[0] for e in errors] == ["recording nightly", "recording nightly"]
