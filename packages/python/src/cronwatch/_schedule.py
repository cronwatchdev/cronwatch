"""Schedules: "0 2 * * *" (cron, five or six fields), "@hourly", or "every
5m". Due times, deadlines, and what a run covers, as schedule.ts has them.
Fire times come from the port of croner in _cron.py, so a Node, a Ruby, and a
Python process sharing one store agree on every due time."""

from __future__ import annotations

import re
import threading
from dataclasses import dataclass, field
from typing import Any

from . import _js, _zone
from ._cron import Cron, CronError
from ._duration import FIRST_DATE_MS, LAST_DATE_MS, parse_duration

__all__ = [
    "CYCLE_MS",
    "EARLY_SLACK_MS",
    "Expectation",
    "ParsedSchedule",
    "expectation",
    "fires_between",
    "interval_due",
    "next_fire",
    "parse_schedule",
    "run_covers",
    "spring_gap",
]

#: How early a run may start and still count for the fire it was meant for.
EARLY_SLACK_MS = 60_000

_EVERY = re.compile(f"every[{_js.WHITESPACE}]+([^\\n\\r\\u2028\\u2029]+)\\Z", re.IGNORECASE | re.ASCII)


@dataclass(frozen=True)
class ParsedSchedule:
    kind: str
    source: str
    #: The IANA timezone a cron is read in, when one was given.
    timezone: str | None = None
    #: For intervals, the period in milliseconds.
    every_ms: float | None = None
    _cron: Cron | None = field(default=None, repr=False, compare=False)

    @property
    def is_cron(self) -> bool:
        return self.kind == "cron"

    @property
    def is_interval(self) -> bool:
        return self.kind == "interval"

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {"kind": self.kind, "source": self.source}
        if self.timezone:
            out["timezone"] = self.timezone
        if self.every_ms is not None:
            out["everyMs"] = self.every_ms
        return out


@dataclass(frozen=True)
class Expectation:
    #: When the next run the schedule asks for is due.
    due_at: int
    #: Missed once now passes this.
    deadline: int

    def to_dict(self) -> dict[str, Any]:
        return {"dueAt": self.due_at, "deadline": self.deadline}


_cache: dict[str, ParsedSchedule] = {}
_cache_lock = threading.Lock()


def parse_schedule(schedule: str, timezone: str | None = None) -> ParsedSchedule:
    """Parsed once per (schedule, timezone) pair and cached. Without a timezone
    the expression is read in the process timezone, like crontab. Vercel and
    GitHub Actions run their crons in UTC, so pass timezone="UTC" for those."""
    key = f"{timezone or ''}|{schedule}"
    with _cache_lock:
        hit = _cache.get(key)
    if hit is not None:
        return hit
    text = _js.trim(schedule)
    every = _EVERY.match(text)
    if every:
        every_ms = parse_duration(every.group(1), "schedule interval")
        if every_ms < 1000:
            raise ValueError(f'schedule "{schedule}" is shorter than one second')
        parsed = ParsedSchedule(kind="interval", source=text, every_ms=every_ms)
    else:
        try:
            cron = Cron(text, timezone or None)
        except CronError as error:
            raise ValueError(f'schedule "{schedule}" is not a cron expression or "every <duration>": {error}') from None
        parsed = ParsedSchedule(kind="cron", source=text, timezone=timezone or None, _cron=cron)
    with _cache_lock:
        _cache[key] = parsed
    return parsed


#: Four hundred Gregorian years: 146,097 days, a whole number of weeks, after
#: which the calendar repeats date for date and weekday for weekday.
CYCLE_MS = 146_097 * 86_400_000
#: Times before the year 400 are asked a cycle or more later, as the SDK asks
#: croner (which misreads a year below 100).
_CRONER_FIRST_MS = _js.date_utc(400, 0, 1)
#: Times from the year 2800 are asked a cycle or more earlier, as the SDK asks
#: croner (which finds no fire past the year 3000, and nor does its port).
_CRONER_LAST_MS = _js.date_utc(2800, 0, 1)


def _runs_after(cron: Cron, count: int, start: int) -> list[int]:
    """The next `count` fires of a cron strictly after `start`, which lies
    within the years 1 to 9999, dropping any after 9999. A time outside the
    years 400 to 2800 is moved by whole 400-year cycles into them, and its
    fires moved back: a time before 400 goes forward, into the same local
    mean time every zone kept then, and one from 2800 goes back, to where the
    zone's present rules already hold."""
    shift = 0
    if start < _CRONER_FIRST_MS:
        shift = -((start - _CRONER_FIRST_MS) // CYCLE_MS) * CYCLE_MS
    elif start >= _CRONER_LAST_MS:
        shift = -((start - _CRONER_LAST_MS) // CYCLE_MS + 1) * CYCLE_MS
    out: list[int] = []
    for fire in cron.next_runs(count, start + shift):
        t = fire - shift
        if t > LAST_DATE_MS:
            break
        out.append(t)
    return out


def _count_from(start: int) -> int | None:
    """A stored time as a cron's fires are counted from it. A start read from
    a foreign or damaged row can be any number: one before the year 1 counts
    from just before its first millisecond, so the first fire of the year 1
    is the next one, and one at or after the last millisecond of 9999 has no
    fire after it at all (None). No fire is ever after 9999."""
    if start >= LAST_DATE_MS:
        return None
    return start if start >= FIRST_DATE_MS else FIRST_DATE_MS - 1


def _fire_after(parsed: ParsedSchedule, start: int) -> int | None:
    """The first fire strictly after `start`, or None when the cron never fires
    again. On the night clocks spring forward every slot the pattern names
    still fires: one whose local time the jump skips is moved forward by the
    gap's length (see spring_gap and _gap_fire_after), so the fires stay
    strictly ascending whichever instant they are counted from."""
    found = _crone_fire_after(parsed, start)
    counted = _count_from(start)
    if counted is None:
        return found
    moved = _gap_fire_after(parsed, counted)
    return moved if moved is not None and (found is None or moved < found) else found


def _crone_fire_after(parsed: ParsedSchedule, start: int) -> int | None:
    """The first fire strictly after `start` as croner counts it, or None when
    the cron never fires again. Croner answers with times in the past when
    asked from inside the hour that repeats when clocks go back, so its
    answers are filtered, and a stretch of nothing but past times is stepped
    over an hour at a time. Asked from inside a spring-forward gap it skips
    the slots moved out of it, which _fire_after adds back."""
    cron = parsed._cron
    if cron is None:
        raise ValueError(f'schedule "{parsed.source}" was not made by parse_schedule')
    counted = _count_from(start)
    if counted is None:
        return None
    probe = counted
    for _ in range(4):
        runs = _runs_after(cron, 8, probe)
        if not runs:
            return None
        for fire in runs:
            if fire > counted:
                return fire
        probe += 3_600_000
    return None


@dataclass(frozen=True)
class SpringGap:
    """A spring-forward jump: `jump_at`, the first minute on the later offset;
    `gap_ms`, the later offset less the earlier one (30 minutes on Lord Howe
    Island, an hour in most zones); `before_ms`, the earlier offset."""

    jump_at: int
    gap_ms: int
    before_ms: int


def spring_gap(timezone: str | None, at: int) -> SpringGap | None:
    """The window a spring-forward jump moves skipped slots into, when `at`
    lies in one: from the jump for the gap's length. None when `at` is not
    within a gap's length after a jump in the three hours before it."""
    jump = _spring_jump(timezone, at)
    return jump if jump is not None and at < jump.jump_at + jump.gap_ms else None


#: How far back a spring-forward jump is looked for: more than any gap a zone has had.
_JUMP_LOOKBACK_MS = 3 * 3_600_000


def _spring_jump(timezone: str | None, at: int) -> SpringGap | None:
    """The spring-forward jump in the three hours up to `at`, found to the
    minute: the first minute on the later offset. None when the offset at
    `at` is not ahead of the offset three hours before."""
    # Every zone kept its local mean time, with no clock change, in the year 1.
    if at - _JUMP_LOOKBACK_MS < FIRST_DATE_MS or at >= LAST_DATE_MS:
        return None
    after = _utc_offset(at, timezone)
    before = _utc_offset(at - _JUMP_LOOKBACK_MS, timezone)
    if after <= before:
        return None
    lo = at - _JUMP_LOOKBACK_MS
    hi = at
    while hi - lo > 60_000:
        mid = lo + (hi - lo) // 2
        if _utc_offset(mid, timezone) == after:
            hi = mid
        else:
            lo = mid
    return SpringGap(jump_at=(hi // 60_000) * 60_000, gap_ms=after - before, before_ms=before)


#: Each pattern read in UTC, which stands for wall-clock time in _gap_fire_after.
_wall_crons: dict[str, Cron] = {}


def _wall_cron(parsed: ParsedSchedule) -> Cron:
    with _cache_lock:
        wall = _wall_crons.get(parsed.source)
        if wall is None:
            wall = Cron(parsed.source, "UTC")
            _wall_crons[parsed.source] = wall
    return wall


def _gap_fire_after(parsed: ParsedSchedule, start: int) -> int | None:
    """The first slot moved out of a spring-forward gap strictly after
    `start`, when `start` lies in that gap's window, else None. A slot whose
    local time the jump skips fires at that local time read on the earlier
    offset, which is the same as moving it forward by the gap's length: 01:20
    on the night London skips 01:00 to 02:00 fires at 02:20 BST (01:20Z).
    Croner moves them the same way when asked from before the jump, but not
    from inside it."""
    gap = spring_gap(parsed.timezone, start)
    if gap is None:
        return None
    # Wall-clock time as if it were UTC: the slots the jump skips run from
    # jump_at + before_ms for gap_ms.
    slots = _runs_after(_wall_cron(parsed), 1, start + gap.before_ms)
    if not slots or slots[0] >= gap.jump_at + gap.before_ms + gap.gap_ms:
        return None
    return slots[0] - gap.before_ms


def fires_between(parsed: ParsedSchedule, start: int, end: int, limit: int) -> list[int] | None:
    """Every fire of a cron strictly after `start` and at or before `end`,
    ascending, or None when there are more than `limit`: the fires next_fire
    gives one after another. Asks for fires in batches, which is far cheaper
    than one next_fire per fire, and drops any that do not move forward (the
    hour that repeats when clocks go back, see _crone_fire_after). Around a
    spring-forward gap, where a batch can skip a moved slot or give one out
    of order, it counts with _fire_after instead. The dashboard's timelines
    draw these."""
    cron = parsed._cron
    if cron is None:
        raise ValueError(f'schedule "{parsed.source}" was not made by parse_schedule')
    out: list[int] = []
    counted = _count_from(start)
    if counted is None:
        return out
    probe = counted
    last = counted
    for _ in range(1000):
        if spring_gap(parsed.timezone, last) is not None:
            # Counting from inside a gap's window: croner would skip its moved slots.
            t = _fire_after(parsed, last)
            if t is None or t > end:
                return out
            out.append(t)
            last = probe = t
            if len(out) > limit:
                return None
            continue
        batch = _runs_after(cron, min(limit + 1 - len(out), 24), probe)
        if not batch:
            return out
        stepped = False
        for t in batch:
            if t <= last:
                continue
            if spring_gap(parsed.timezone, t) is not None:
                # The batch reached a gap's window, where its order is croner's:
                # the next fire is counted from the last one, and the rest with it.
                following = _fire_after(parsed, last)
                if following is None or following > end:
                    return out
                out.append(following)
                last = probe = following
                if len(out) > limit:
                    return None
                stepped = True
                break
            if t > end:
                return out
            out.append(t)
            last = t
            if len(out) > limit:
                return None
        if stepped:
            continue
        finish = batch[-1]
        probe = finish if finish > probe else probe + 3_600_000
    return out


def next_fire(parsed: ParsedSchedule, start: int, last_run_at: int | None) -> int | None:
    """The next time the schedule fires strictly after `start`. For an
    interval, counted from the last run's start when there is one (see
    interval_due for a run that outlasted its period)."""
    if parsed.kind == "interval":
        base = start if last_run_at is None else last_run_at
        return base + parsed.every_ms  # type: ignore[operator, return-value]
    return _fire_after(parsed, start)


def interval_due(every_ms: float, started_at: int, finished_at: int | None) -> int:
    """When an interval of `every_ms` is next due after a run: a period after
    it started, or, when it finished (`finished_at` not None) later than
    that, a period after it finished. A run that outlasts its period is not
    followed by a missed alert the moment it ends."""
    due = started_at + every_ms
    return finished_at + every_ms if finished_at is not None and finished_at > due else due  # type: ignore[return-value]


def expectation(
    parsed: ParsedSchedule,
    last_run_at: int | None,
    registered_at: int,
    grace_ms: float,
    last_finished_at: int | None = None,
) -> Expectation | None:
    """When the schedule next wants a run, given the last one. For a cron that
    is the first fire the last run does not already cover; with no run yet,
    the first fire at or after registration. For an interval it is the last
    run's start (or registration) plus the interval, or its finish plus the
    interval when it finished (`last_finished_at`, None while it runs) later
    than that (interval_due). None for a cron that never fires again.

    Counting forward from the last run, rather than back from now, is what
    lets a job whose period is shorter than its grace be missed at all, and it
    works for a cron that fires once a year or less."""
    if parsed.kind == "interval":
        every: float = parsed.every_ms  # type: ignore[assignment]
        due_at: float | None = registered_at + every if last_run_at is None else interval_due(every, last_run_at, last_finished_at)
    elif last_run_at is None:
        due_at = _fire_after(parsed, registered_at - 1)
    else:
        due_at = _due_after_run(parsed, last_run_at)
    return None if due_at is None else Expectation(due_at=due_at, deadline=due_at + grace_ms)  # type: ignore[arg-type]


def _due_after_run(parsed: ParsedSchedule, started_at: int) -> int | None:
    """The first fire that a run starting at `started_at` does not cover. A
    start before the year 1 covers none of them, so the first fire of the
    year 1 is due; after 9999 there is none (see _count_from)."""
    # A fire at or before the start is covered by the run itself.
    upcoming = _fire_after(parsed, started_at)
    # Bounded: a gap holds at most a few hours of moved slots.
    for _ in range(100_000):
        if upcoming is None:
            return None
        following = _fire_after(parsed, upcoming)
        if not (run_covers(started_at, upcoming, following) or _in_spring_forward_gap(parsed, started_at, upcoming)):
            return upcoming
        upcoming = following
    return upcoming


def run_covers(started_at: int, due_at: int, following_at: int | None = None) -> bool:
    """Whether a run starting at `started_at` covers the fire at `due_at`. A
    minute of slack before the tick absorbs schedulers that fire a touch
    early. When the fire after `due_at` is known, the slack is at most half
    the gap between the two, so one run of an every-minute cron never covers
    two fires."""
    slack = EARLY_SLACK_MS if following_at is None else min(EARLY_SLACK_MS, (following_at - due_at) // 2)
    return started_at >= due_at - slack


def _in_spring_forward_gap(parsed: ParsedSchedule, started_at: int, fire_at: int) -> bool:
    """On the night clocks spring forward, a slot whose local time does not
    exist (02:30 when 02:00 jumps to 03:00) fires moved forward by the gap
    (03:30; see _gap_fire_after), while vixie cron runs every skipped job
    once, at the jump itself (03:00). A run that starts at or after the jump
    (less a minute of slack), and before such a moved fire, is taken to cover
    it, so neither scheduler's run is reported as missed. Only a moved slot
    that is not also a slot of its own on the later offset counts: a cron
    that fires every 10 minutes moves 02:10 onto its own 03:10, which was not
    skipped."""
    gap = spring_gap(parsed.timezone, fire_at)
    if gap is None or started_at < gap.jump_at - EARLY_SLACK_MS or started_at >= fire_at:
        return False
    wall = _wall_cron(parsed)

    def fires(at: int) -> bool:
        slots = _runs_after(wall, 1, at - 1)
        return bool(slots) and slots[0] == at

    return fires(fire_at + gap.before_ms) and not fires(fire_at + gap.before_ms + gap.gap_ms)


def _utc_offset(at: int, timezone: str | None) -> int:
    """Milliseconds the zone's wall clock is ahead of UTC at `at`."""
    return _zone.offset(at // 1000, timezone) * 1000
