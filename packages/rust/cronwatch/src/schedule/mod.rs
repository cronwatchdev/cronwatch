//! The SDK's `duration.ts` and `schedule.ts`: durations ("15m", "1h30m", a
//! number of milliseconds) parsed and written as the SDK does, and
//! schedules ("0 2 * * *", "@hourly", "every 5m") with their fire times, due
//! times, deadlines, and what a run covers. Cron fire times come from the
//! port of croner in the `cron` module, so a Rust process and a Node, Ruby,
//! Python, PHP, or Go process sharing one store agree on every due time.
//!
//! As in the SDK, a date no month has never fires and croner's one-time
//! dates are refused (see the `cron` module).

pub(crate) mod cron;
mod duration;

use std::collections::HashMap;
use std::sync::{Arc, LazyLock, Mutex};

use jiff::tz::TimeZone;

use crate::js::{FIRST_DATE_MS, LAST_DATE_MS, Object, Value, floor_div, is_space, trim};
pub(crate) use duration::format_relative;
use duration::parse_duration_text;
pub(crate) use duration::{format_duration, parse_duration};

/// How early a run may start and still count for the fire it was meant for.
pub(crate) const EARLY_SLACK_MS: i64 = 60_000;

/// The longest interval kept, 2^53 ms: added to any time the SDK meets it
/// stays well inside an `i64`. A longer "every" is read as this long, which
/// fires no sooner in any run's lifetime.
pub(crate) const MAX_INTERVAL_MS: i64 = 1 << 53;

/// What kind of schedule a `Parsed` is.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum ScheduleKind {
    Cron,
    Interval,
}

impl ScheduleKind {
    /// The SDK's name for the kind: "cron" or "interval".
    #[allow(dead_code)] // only the tests read it
    pub(crate) fn as_str(self) -> &'static str {
        match self {
            ScheduleKind::Cron => "cron",
            ScheduleKind::Interval => "interval",
        }
    }
}

/// A schedule as `parseSchedule` returns it: plain data (`to_value` is the
/// SDK's JSON of it), with the croner expression behind a cron kept out of
/// view.
#[derive(Clone, Debug)]
pub(crate) struct Parsed {
    pub(crate) kind: ScheduleKind,
    #[allow(dead_code)] // only the tests read it
    pub(crate) source: String,
    /// The IANA zone a cron is read in, "" when none was given.
    #[allow(dead_code)] // only the tests read it
    pub(crate) timezone: String,
    /// An interval's period in milliseconds.
    pub(crate) every_ms: i64,
    cron: Option<cron::Cron>,
    /// The same pattern read in UTC, which stands for wall-clock time when
    /// slots are moved out of a spring-forward gap (`gap_fire_after`).
    wall: Option<cron::Cron>,
}

impl Parsed {
    /// Whether this is "every <duration>" rather than a cron.
    pub(crate) fn is_interval(&self) -> bool {
        self.kind == ScheduleKind::Interval
    }

    /// The SDK's JSON of a parsed schedule: `{kind, source, timezone}` for a
    /// cron (timezone only when given) and `{kind, source, everyMs}` for an
    /// interval.
    #[allow(dead_code)] // only the tests read it
    pub(crate) fn to_value(&self) -> Value {
        let mut o = Object::new().with("kind", self.kind.as_str()).with("source", self.source.as_str());
        if self.is_interval() {
            o.set("everyMs", self.every_ms);
        } else if !self.timezone.is_empty() {
            o.set("timezone", self.timezone.as_str());
        }
        Value::Object(o)
    }

    /// The SDK's `runsAfter`: croner's `nextRuns` for a cron, from a `start`
    /// within the years 1 to 9999, dropping any fire after 9999; nothing for
    /// an interval. A time croner cannot answer for (it misreads a year
    /// below 100 and finds no fire past 3000) is moved by whole 400-year
    /// cycles into the years it can, and its fires moved back: a time before
    /// 400 goes forward, into the same local mean time every zone kept then,
    /// and one from 2800 goes back, to where the zone's present rules
    /// already hold. The SDK does the same, so the two agree on every fire.
    fn next_runs(&self, count: usize, start: i64) -> Vec<i64> {
        match self.cron.as_ref() {
            Some(c) => runs_after(c, count, start),
            None => Vec::new(),
        }
    }
}

/// The SDK's `runsAfter` for any of a schedule's crons (see `next_runs`).
fn runs_after(c: &cron::Cron, count: usize, start: i64) -> Vec<i64> {
    let shift = if start < CRONER_FIRST_MS {
        (CRONER_FIRST_MS - start + CYCLE_MS - 1) / CYCLE_MS * CYCLE_MS
    } else if start >= CRONER_LAST_MS {
        -((start - CRONER_LAST_MS) / CYCLE_MS + 1) * CYCLE_MS
    } else {
        0
    };
    let mut out = Vec::with_capacity(count);
    for t in c.next_runs(count, start + shift) {
        let t = t - shift;
        if t > LAST_DATE_MS {
            break;
        }
        out.push(t);
    }
    out
}

/// Four hundred Gregorian years: 146,097 days, a whole number of weeks,
/// after which the calendar repeats date for date and weekday for weekday.
const CYCLE_MS: i64 = 146_097 * 86_400_000;
/// 0400-01-01T00:00:00Z: an earlier time is asked a cycle or more later.
const CRONER_FIRST_MS: i64 = -49_544_438_400_000;
/// 2800-01-01T00:00:00Z: a later time is asked a cycle or more earlier.
const CRONER_LAST_MS: i64 = 26_192_246_400_000;

/// The SDK's `countFrom`: a stored time as a cron's fires are counted from
/// it. A start read from a foreign or damaged row can be any number: one
/// before the year 1 counts from just before its first millisecond, so the
/// first fire of the year 1 is the next one, and one at or after the last
/// millisecond of 9999 has no fire after it at all (None). No fire is ever
/// after 9999.
fn count_from(from: i64) -> Option<i64> {
    if from >= LAST_DATE_MS {
        return None;
    }
    Some(from.max(FIRST_DATE_MS - 1))
}

static CACHE: LazyLock<Mutex<HashMap<String, Arc<Parsed>>>> = LazyLock::new(Default::default);

/// The SDK's `parseSchedule`: "0 2 * * *" (a cron of five or six fields), a
/// nickname ("@hourly"), or "every 5m". Each (schedule, timezone) pair is
/// parsed once and kept. Without a timezone a cron is read in the process's
/// zone, as crontab reads the system's; Vercel and GitHub Actions run their
/// crons in UTC, so pass "UTC" for those. The errors are the SDK's, word for
/// word.
///
/// A zone the database does not have is refused here with the message
/// croner throws for it when asked for a fire time, since the SDK reads the
/// zone only then: call `is_timezone` first to report a bad zone as the
/// SDK's client does.
pub(crate) fn parse(schedule: &str, timezone: &str) -> Result<Arc<Parsed>, String> {
    let key = format!("{timezone}|{schedule}");
    if let Some(hit) = CACHE.lock().unwrap_or_else(|e| e.into_inner()).get(&key) {
        return Ok(hit.clone());
    }
    let parsed = Arc::new(parse_uncached(schedule, timezone)?);
    let mut cache = CACHE.lock().unwrap_or_else(|e| e.into_inner());
    // A long-running process parses a handful of schedules; the bound only
    // keeps one fed endless distinct schedules from growing without end.
    if cache.len() >= 1000 {
        cache.clear();
    }
    cache.insert(key, parsed.clone());
    Ok(parsed)
}

fn parse_uncached(schedule: &str, timezone: &str) -> Result<Parsed, String> {
    let text = trim(schedule);
    if let Some(rest) = every(text) {
        let ms = parse_duration_text(rest, "schedule interval")?;
        if ms < 1000.0 {
            return Err(format!("schedule \"{schedule}\" is shorter than one second"));
        }
        // JavaScript adds an interval of any size to a time; an i64 would
        // wrap past about 292 million years, so a longer one is held at
        // 2^53 ms, still some 285,000 years, which no run outlives.
        return Ok(Parsed {
            kind: ScheduleKind::Interval,
            source: text.to_string(),
            timezone: String::new(),
            every_ms: ms.min(MAX_INTERVAL_MS as f64) as i64,
            cron: None,
            wall: None,
        });
    }
    let pattern_zone = if timezone.is_empty() { TimeZone::system() } else { TimeZone::UTC };
    let c = cron::Cron::new(text, pattern_zone)
        .map_err(|e| format!("schedule \"{schedule}\" is not a cron expression or \"every <duration>\": {e}"))?;
    let wall = cron::Cron::new(text, TimeZone::UTC)?;
    let c = if timezone.is_empty() {
        c
    } else {
        let tz = cron::load_zone(timezone).map_err(|_| {
            format!(
                "CronDate: Failed to convert date to timezone '{timezone}'. This may happen with invalid timezone names or dates. \
                 Original error: toTZ: Invalid timezone '{timezone}' or date. Please provide a valid IANA timezone (e.g., 'America/New_York', 'Europe/Stockholm'). \
                 Original error: Invalid time zone specified: {timezone}"
            )
        })?;
        cron::Cron::new(text, tz)?
    };
    Ok(Parsed {
        kind: ScheduleKind::Cron,
        source: text.to_string(),
        timezone: timezone.to_string(),
        every_ms: 0,
        cron: Some(c),
        wall: Some(wall),
    })
}

/// `/^every\s+(.+)$/i`: "every" in any ASCII case, whitespace, and the rest,
/// which must hold no line terminator (JavaScript's "." matches none).
fn every(text: &str) -> Option<&str> {
    let head = text.get(..5)?;
    if !head.eq_ignore_ascii_case("every") {
        return None;
    }
    let rest = &text[5..];
    let trimmed = rest.trim_start_matches(is_space);
    if trimmed.len() == rest.len() || trimmed.is_empty() || trimmed.contains(['\n', '\r', '\u{2028}', '\u{2029}']) {
        return None;
    }
    Some(trimmed)
}

/// The first fire strictly after `from`, or None when the cron never fires
/// again. On the night clocks spring forward every slot the pattern names
/// still fires: one whose local time the jump skips is moved forward by the
/// gap's length (see `spring_gap` and `gap_fire_after`), so the fires stay
/// strictly ascending whichever instant they are counted from.
pub(crate) fn fire_after(p: &Parsed, from: i64) -> Option<i64> {
    let found = crone_fire_after(p, from);
    let Some(start) = count_from(from) else {
        return found;
    };
    match gap_fire_after(p, start) {
        Some(moved) if found.is_none_or(|f| moved < f) => Some(moved),
        _ => found,
    }
}

/// The first fire strictly after `from` as croner counts it, or None when
/// the cron never fires again. Croner answers with times in the past when
/// asked from inside the hour that repeats when clocks go back, so its
/// answers are filtered, and a stretch of nothing but past times is stepped
/// over an hour at a time. Asked from inside a spring-forward gap it skips
/// the slots moved out of it, which `fire_after` adds back.
fn crone_fire_after(p: &Parsed, from: i64) -> Option<i64> {
    let start = count_from(from)?;
    let mut probe = start;
    for _ in 0..4 {
        let runs = p.next_runs(8, probe);
        if runs.is_empty() {
            return None;
        }
        if let Some(&t) = runs.iter().find(|&&t| t > start) {
            return Some(t);
        }
        probe += 3_600_000;
    }
    None
}

/// A spring-forward jump: `jump_at` is the first minute on the later
/// offset, `gap_ms` the later offset less the earlier one (30 minutes on
/// Lord Howe Island, an hour in most zones), and `before_ms` the earlier
/// offset.
#[derive(Clone, Copy, Debug)]
struct Jump {
    jump_at: i64,
    gap_ms: i64,
    before_ms: i64,
}

/// How far back a spring-forward jump is looked for: more than any gap a
/// zone has had.
const JUMP_LOOKBACK_MS: i64 = 3 * 3_600_000;

/// The SDK's `springJump`: the spring-forward jump in the three hours up
/// to `at`, found to the minute. None when the offset at `at` is not ahead
/// of the offset three hours before.
fn spring_jump(tz: &TimeZone, at: i64) -> Option<Jump> {
    // Every zone kept its local mean time, with no clock change, in the year 1.
    if at.saturating_sub(JUMP_LOOKBACK_MS) < FIRST_DATE_MS || at >= LAST_DATE_MS {
        return None;
    }
    let after = utc_offset(at, tz);
    let before = utc_offset(at - JUMP_LOOKBACK_MS, tz);
    if after <= before {
        return None;
    }
    let (mut lo, mut hi) = (at - JUMP_LOOKBACK_MS, at);
    while hi - lo > 60_000 {
        let mid = lo + (hi - lo) / 2;
        if utc_offset(mid, tz) == after {
            hi = mid;
        } else {
            lo = mid;
        }
    }
    Some(Jump { jump_at: floor_div(hi, 60_000) * 60_000, gap_ms: after - before, before_ms: before })
}

/// The SDK's `springGap`: the jump whose window `at` lies in, the UTC span
/// from the jump for the gap's length that slots the jump skips are moved
/// into. None for an interval, or when `at` is in no such window.
fn spring_gap(p: &Parsed, at: i64) -> Option<Jump> {
    let jump = spring_jump(p.cron.as_ref()?.zone(), at)?;
    (at < jump.jump_at + jump.gap_ms).then_some(jump)
}

/// The first slot moved out of a spring-forward gap strictly after
/// `start`, when `start` lies in that gap's window, else None. A slot whose
/// local time the jump skips fires at that local time read on the earlier
/// offset, which is the same as moving it forward by the gap's length:
/// 01:20 on the night London skips 01:00 to 02:00 fires at 02:20 BST
/// (01:20Z). Croner moves them the same way when asked from before the
/// jump, but not from inside it.
fn gap_fire_after(p: &Parsed, start: i64) -> Option<i64> {
    let gap = spring_gap(p, start)?;
    // Wall-clock time as if it were UTC: the slots the jump skips run from
    // jump_at + before_ms for gap_ms.
    let slot = *runs_after(p.wall.as_ref()?, 1, start + gap.before_ms).first()?;
    if slot >= gap.jump_at + gap.before_ms + gap.gap_ms {
        return None;
    }
    Some(slot - gap.before_ms)
}

/// Every fire of a cron strictly after `from` and at or before `to`,
/// ascending, or None when there are more than `limit`: the fires
/// `next_fire` gives one after another. It asks for fires in batches, far
/// cheaper than one `next_fire` each, and drops any that do not move
/// forward (the hour that repeats when clocks go back, see
/// `crone_fire_after`). Around a spring-forward gap, where croner's batch
/// can skip a moved slot or give one out of order, it counts with
/// `fire_after` instead.
pub(crate) fn fires_between(p: &Parsed, from: i64, to: i64, limit: usize) -> Option<Vec<i64>> {
    let mut out = Vec::new();
    let Some(start) = count_from(from) else {
        return Some(out);
    };
    let (mut probe, mut last) = (start, start);
    'batches: for _ in 0..1000 {
        if spring_gap(p, last).is_some() {
            // Counting from inside a gap's window: croner would skip its moved slots.
            match fire_after(p, last) {
                Some(t) if t <= to => {
                    out.push(t);
                    (last, probe) = (t, t);
                    if out.len() > limit {
                        return None;
                    }
                    continue;
                }
                _ => return Some(out),
            }
        }
        let batch = p.next_runs((limit + 1 - out.len()).min(24), probe);
        let Some(&end) = batch.last() else {
            return Some(out);
        };
        for t in batch {
            if t <= last {
                continue;
            }
            if spring_gap(p, t).is_some() {
                // The batch reached a gap's window, where its order is
                // croner's: the next fire is counted from the last one, and
                // the rest with it.
                match fire_after(p, last) {
                    Some(next) if next <= to => {
                        out.push(next);
                        (last, probe) = (next, next);
                        if out.len() > limit {
                            return None;
                        }
                        continue 'batches;
                    }
                    _ => return Some(out),
                }
            }
            if t > to {
                return Some(out);
            }
            out.push(t);
            last = t;
            if out.len() > limit {
                return None;
            }
        }
        probe = if end > probe { end } else { probe + 3_600_000 };
    }
    Some(out)
}

/// The next time the schedule fires strictly after `from`; for an interval,
/// counted from the last run's start when there is one (see `interval_due`
/// for a run that outlasted its period). None when a cron never fires
/// again.
pub(crate) fn next_fire(p: &Parsed, from: i64, last_run_at: Option<i64>) -> Option<i64> {
    if p.is_interval() {
        return Some(last_run_at.unwrap_or(from).saturating_add(p.every_ms));
    }
    fire_after(p, from)
}

/// The SDK's `intervalDue`: when an interval of `every_ms` is next due
/// after a run, a period after it started or, when it finished
/// (`finished_at` is Some) later than that, a period after it finished. A
/// run that outlasts its period is not followed by a missed alert the
/// moment it ends.
pub(crate) fn interval_due(every_ms: i64, started_at: i64, finished_at: Option<i64>) -> i64 {
    let due = started_at.saturating_add(every_ms);
    match finished_at {
        Some(f) if f > due => f.saturating_add(every_ms),
        _ => due,
    }
}

/// When the next run is due, and when it is missed.
#[derive(Clone, Copy, Debug, PartialEq)]
pub(crate) struct Expectation {
    pub(crate) due_at: i64,
    /// Missed once now passes this.
    pub(crate) deadline: f64,
}

/// The SDK's `expectation()`: when the schedule next wants a run, given the
/// last one. For a cron that is the first fire the last run does not
/// already cover; with no run yet, the first fire at or after registration.
/// For an interval it is the last run's start (or registration) plus the
/// interval, or its finish plus the interval when it finished
/// (`last_finished_at`, None while it runs) later than that
/// (`interval_due`). None for a cron that never fires again.
///
/// Counting forward from the last run, rather than back from now, is what
/// lets a job whose period is shorter than its grace be missed at all, and
/// it works for a cron that fires once a year or less.
pub(crate) fn expect(
    p: &Parsed,
    last_run_at: Option<i64>,
    registered_at: i64,
    grace_ms: f64,
    last_finished_at: Option<i64>,
) -> Option<Expectation> {
    let due = if p.is_interval() {
        match last_run_at {
            None => registered_at.saturating_add(p.every_ms),
            Some(last) => interval_due(p.every_ms, last, last_finished_at),
        }
    } else {
        match last_run_at {
            None => fire_after(p, registered_at.saturating_sub(1))?,
            Some(last) => due_after_run(p, last)?,
        }
    };
    Some(Expectation { due_at: due, deadline: due as f64 + grace_ms })
}

/// The first fire of a cron that a run starting at `started_at` does not
/// cover, or None when there is none. A start before the year 1 covers none
/// of them, so the first fire of the year 1 is due; after 9999 there is none
/// (see `count_from`).
pub(crate) fn due_after_run(p: &Parsed, started_at: i64) -> Option<i64> {
    // A fire at or before the start is covered by the run itself.
    let mut next = fire_after(p, started_at)?;
    // Bounded: a gap holds at most a few hours of moved slots.
    for _ in 0..100_000 {
        let following = fire_after(p, next);
        if !(run_covers(started_at, next, following) || in_spring_forward_gap(p, started_at, next)) {
            return Some(next);
        }
        next = following?;
    }
    Some(next)
}

/// Whether a run starting at `started_at` covers the fire at `due_at`. A
/// minute of slack before the tick absorbs schedulers that fire a touch
/// early. When the fire after `due_at` is known, the slack is at most half
/// the gap between the two, so one run of an every-minute cron never covers
/// two fires.
pub(crate) fn run_covers(started_at: i64, due_at: i64, following_at: Option<i64>) -> bool {
    let slack = match following_at {
        Some(f) => EARLY_SLACK_MS.min(floor_div(f - due_at, 2)),
        None => EARLY_SLACK_MS,
    };
    started_at >= due_at - slack
}

/// On the night clocks spring forward, a slot whose local time does not
/// exist (02:30 when 02:00 jumps to 03:00) fires moved forward by the gap
/// (03:30; see `gap_fire_after`), while vixie cron runs every skipped job
/// once, at the jump itself (03:00). A run that starts at or after the jump
/// (less a minute of slack), and before such a moved fire, is taken to
/// cover it, so neither scheduler's run is reported as missed. Only a moved
/// slot that is not also a slot of its own on the later offset counts: a
/// cron that fires every 10 minutes moves 02:10 onto its own 03:10, which
/// was not skipped.
fn in_spring_forward_gap(p: &Parsed, started_at: i64, fire_at: i64) -> bool {
    let (Some(gap), Some(wall)) = (spring_gap(p, fire_at), p.wall.as_ref()) else {
        return false;
    };
    if started_at < gap.jump_at - EARLY_SLACK_MS || started_at >= fire_at {
        return false;
    }
    let fires = |at: i64| runs_after(wall, 1, at - 1).first() == Some(&at);
    fires(fire_at + gap.before_ms) && !fires(fire_at + gap.before_ms + gap.gap_ms)
}

/// The milliseconds the zone's wall clock is ahead of UTC at `at`.
fn utc_offset(at: i64, tz: &TimeZone) -> i64 {
    cron::offset(floor_div(at, 1000), tz) * 1000
}

/// Whether `new Intl.DateTimeFormat("en-US", { timeZone })` accepts the
/// name: an IANA zone, matched without regard to case, "UTC", or a fixed
/// offset such as "+05:30".
pub(crate) fn is_timezone(name: &str) -> bool {
    !name.is_empty() && cron::load_zone(name).is_ok()
}

/// The zone an IANA name names, matched without regard to case as `Intl`
/// matches it; "" is the process's own zone.
#[allow(dead_code)] // the scheduler integrations use it (phase 4)
pub(crate) fn load_zone(name: &str) -> Result<TimeZone, String> {
    cron::load_zone(name).map_err(|_| format!("timezone \"{name}\" is not an IANA timezone"))
}

#[cfg(test)]
mod conformance_tests;
#[cfg(test)]
mod fuzz_tests;
#[cfg(test)]
mod tests;
