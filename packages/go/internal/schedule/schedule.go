package schedule

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"cronwatch.dev/go/internal/js"
	"cronwatch.dev/go/internal/schedule/cron"
)

// EarlySlackMs is how early a run may start and still count for the fire it
// was meant for.
const EarlySlackMs = 60_000

// Parsed is a schedule as parseSchedule returns it: plain data (JSValue is
// the SDK's JSON of it), with the croner expression behind a cron kept out
// of view.
type Parsed struct {
	// "cron" or "interval".
	Kind   string
	Source string
	// The IANA zone a cron is read in, "" when none was given.
	Timezone string
	// An interval's period in milliseconds.
	EveryMs int64

	cron *cron.Cron
	// The pattern read in UTC, which stands for wall-clock time in
	// gapFireAfter and inSpringForwardGap.
	wall *cron.Cron
}

// JSValue is the SDK's JSON of a parsed schedule: {kind, source, timezone}
// for a cron (timezone only when given) and {kind, source, everyMs} for an
// interval.
func (p *Parsed) JSValue() any {
	o := js.NewObject("kind", p.Kind, "source", p.Source)
	if p.Kind == "interval" {
		o.Set("everyMs", p.EveryMs)
	} else if p.Timezone != "" {
		o.Set("timezone", p.Timezone)
	}
	return o
}

// MaxIntervalMs is the longest interval kept, 2^53 ms: added to any time
// the SDK meets it stays well inside an int64. A longer "every" is read as
// this long, which fires no sooner in any run's lifetime.
const MaxIntervalMs = 1 << 53

var (
	cacheMu sync.Mutex
	cache   = map[string]*Parsed{}
)

// Parse is the SDK's parseSchedule: "0 2 * * *" (a cron of five or six
// fields), a nickname ("@hourly"), or "every 5m". Each (schedule, timezone)
// pair is parsed once and kept. Without a timezone a cron is read in the
// process's zone (time.Local), as crontab reads the system's; Vercel and
// GitHub Actions run their crons in UTC, so pass "UTC" for those. The
// errors are the SDK's, word for word.
//
// A zone the database does not have is refused here with the message
// croner throws for it when asked for a fire time, since the SDK reads the
// zone only then: call IsTimezone first to report a bad zone as the SDK's
// client does.
func Parse(schedule, timezone string) (*Parsed, error) {
	key := timezone + "|" + schedule
	cacheMu.Lock()
	hit := cache[key]
	cacheMu.Unlock()
	if hit != nil {
		return hit, nil
	}
	parsed, err := parse(schedule, timezone)
	if err != nil {
		return nil, err
	}
	cacheMu.Lock()
	// A long-running process parses a handful of schedules; the bound only
	// keeps one fed endless distinct schedules from growing without end.
	if len(cache) >= 1000 {
		cache = map[string]*Parsed{}
	}
	cache[key] = parsed
	cacheMu.Unlock()
	return parsed, nil
}

func parse(schedule, timezone string) (*Parsed, error) {
	text := js.Trim(schedule)
	if rest, ok := every(text); ok {
		ms, err := ParseDuration(rest, "schedule interval")
		if err != nil {
			return nil, err
		}
		if ms < 1000 {
			return nil, fmt.Errorf("schedule \"%s\" is shorter than one second", schedule)
		}
		// JavaScript adds an interval of any size to a time; an int64 would
		// wrap past about 292 million years (and float64 to int64 is not
		// defined past it), so a longer one is held at 2^53 ms, still some
		// 285,000 years, which no run outlives.
		return &Parsed{Kind: "interval", Source: text, EveryMs: int64(min(ms, MaxIntervalMs))}, nil
	}
	c, err := cron.New(text, nil)
	if err != nil {
		return nil, fmt.Errorf("schedule \"%s\" is not a cron expression or \"every <duration>\": %s", schedule, err.Error())
	}
	wall, err := cron.New(text, time.UTC)
	if err != nil {
		return nil, err
	}
	if timezone != "" {
		loc, err := cron.LoadZone(timezone)
		if err != nil {
			return nil, errors.New("CronDate: Failed to convert date to timezone '" + timezone + "'. This may happen with invalid timezone names or dates. " +
				"Original error: toTZ: Invalid timezone '" + timezone + "' or date. Please provide a valid IANA timezone (e.g., 'America/New_York', 'Europe/Stockholm'). " +
				"Original error: Invalid time zone specified: " + timezone)
		}
		if c, err = cron.New(text, loc); err != nil {
			return nil, err
		}
	}
	return &Parsed{Kind: "cron", Source: text, Timezone: timezone, cron: c, wall: wall}, nil
}

// every is /^every\s+(.+)$/i: "every" in any ASCII case, whitespace, and
// the rest, which must hold no line terminator (JavaScript's "." matches
// none).
func every(text string) (string, bool) {
	if len(text) < 5 || !strings.EqualFold(text[:5], "every") || strings.ContainsFunc(text[:5], func(r rune) bool { return r > 127 }) {
		return "", false
	}
	rest := text[5:]
	trimmed := strings.TrimLeftFunc(rest, js.IsSpace)
	if trimmed == rest || trimmed == "" || strings.ContainsAny(trimmed, "\n\r  ") {
		return "", false
	}
	return trimmed, true
}

// cycleMs is four hundred Gregorian years: 146,097 days, a whole number of
// weeks, after which the calendar repeats date for date and weekday for
// weekday.
const cycleMs int64 = 146_097 * 86_400_000

var (
	// Croner misreads a year below 100, so the SDK asks it about any time
	// before the year 400 a cycle or more later.
	cronerFirstMs = js.DateUTC(400, 0, 1, 0, 0, 0, 0)
	// Croner finds no fire past the year 3000, so the SDK asks it about any
	// time from the year 2800 a cycle or more earlier.
	cronerLastMs = js.DateUTC(2800, 0, 1, 0, 0, 0, 0)
)

// runsAfter is the SDK's runsAfter: the next n fires of a cron strictly
// after from, which lies within the years 1 to 9999, dropping any after
// 9999. A time croner cannot answer for is moved by whole 400-year cycles
// into the years it can, and its fires moved back, so this port answers
// as the SDK does.
func runsAfter(c *cron.Cron, n int, from int64) []int64 {
	var shift int64
	if from < cronerFirstMs {
		shift = (cronerFirstMs - from + cycleMs - 1) / cycleMs * cycleMs
	} else if from >= cronerLastMs {
		shift = -((from-cronerLastMs)/cycleMs + 1) * cycleMs
	}
	runs := c.NextRuns(n, from+shift)
	out := make([]int64, 0, len(runs))
	for _, t := range runs {
		t -= shift
		if t > js.LastDateMs {
			break
		}
		out = append(out, t)
	}
	return out
}

// countFrom is the SDK's countFrom: a stored time as a cron's fires are
// counted from it. A start read from a foreign or damaged row can be any
// number: one before the year 1 counts from just before its first
// millisecond, so the first fire of the year 1 is the next one, and one at
// or after the last millisecond of 9999 has no fire after it (false).
func countFrom(from int64) (int64, bool) {
	if from >= js.LastDateMs {
		return 0, false
	}
	if from < js.FirstDateMs {
		return js.FirstDateMs - 1, true
	}
	return from, true
}

// fireAfter is the first fire strictly after from, or false when the cron
// never fires again. On the night clocks spring forward every slot the
// pattern names still fires: one whose local time the jump skips is moved
// forward by the gap's length (see springGap and gapFireAfter), so the
// fires stay strictly ascending whichever instant they are counted from.
func fireAfter(p *Parsed, from int64) (int64, bool) {
	found, ok := croneFireAfter(p, from)
	start, inRange := countFrom(from)
	if !inRange {
		return found, ok
	}
	if moved, isMoved := gapFireAfter(p, start); isMoved && (!ok || moved < found) {
		return moved, true
	}
	return found, ok
}

// croneFireAfter is the first fire strictly after from as croner counts
// it, or false when the cron never fires again. Croner answers with times
// in the past when asked from inside the hour that repeats when clocks go
// back, so its answers are filtered, and a stretch of nothing but past
// times is stepped over an hour at a time. Asked from inside a
// spring-forward gap it skips the slots moved out of it, which fireAfter
// adds back.
func croneFireAfter(p *Parsed, from int64) (int64, bool) {
	start, ok := countFrom(from)
	if !ok {
		return 0, false
	}
	probe := start
	for attempt := 0; attempt < 4; attempt++ {
		runs := runsAfter(p.cron, 8, probe)
		if len(runs) == 0 {
			return 0, false
		}
		for _, t := range runs {
			if t > start {
				return t, true
			}
		}
		probe += 3_600_000
	}
	return 0, false
}

// gap is a spring-forward jump: jumpAt, the first minute on the later
// offset; gapMs, the later offset less the earlier one (30 minutes on Lord
// Howe Island, an hour in most zones); and beforeMs, the earlier offset.
type gap struct {
	jumpAt, gapMs, beforeMs int64
}

// jumpLookbackMs is how far back a spring-forward jump is looked for: more
// than any gap a zone has had.
const jumpLookbackMs = 3 * 3_600_000

// springGap is the window a spring-forward jump moves skipped slots into,
// when at lies in one: from the jump for the gap's length. False when at is
// not within a gap's length after a jump in the three hours before it.
func springGap(loc *time.Location, at int64) (gap, bool) {
	g, ok := springJump(loc, at)
	if !ok || at >= g.jumpAt+g.gapMs {
		return gap{}, false
	}
	return g, true
}

// springJump is the spring-forward jump in the three hours up to at, found
// to the minute: the first minute on the later offset. False when the
// offset at at is not ahead of the offset three hours before.
func springJump(loc *time.Location, at int64) (gap, bool) {
	// Every zone kept its local mean time, with no clock change, in the year 1.
	if at-jumpLookbackMs < js.FirstDateMs || at >= js.LastDateMs {
		return gap{}, false
	}
	after := utcOffset(at, loc)
	before := utcOffset(at-jumpLookbackMs, loc)
	if after <= before {
		return gap{}, false
	}
	lo, hi := at-jumpLookbackMs, at
	for hi-lo > 60_000 {
		mid := lo + (hi-lo)/2
		if utcOffset(mid, loc) == after {
			hi = mid
		} else {
			lo = mid
		}
	}
	return gap{jumpAt: js.FloorDiv(hi, 60_000) * 60_000, gapMs: after - before, beforeMs: before}, true
}

// gapFireAfter is the first slot moved out of a spring-forward gap strictly
// after start, when start lies in that gap's window, else false. A slot
// whose local time the jump skips fires at that local time read on the
// earlier offset, which is the same as moving it forward by the gap's
// length: 01:20 on the night London skips 01:00 to 02:00 fires at 02:20
// BST (01:20Z). Croner moves them the same way when asked from before the
// jump, but not from inside it.
func gapFireAfter(p *Parsed, start int64) (int64, bool) {
	g, ok := springGap(zoneOf(p), start)
	if !ok {
		return 0, false
	}
	// Wall-clock time as if it were UTC: the slots the jump skips run from
	// jumpAt + beforeMs for gapMs.
	slots := runsAfter(p.wall, 1, start+g.beforeMs)
	if len(slots) == 0 || slots[0] >= g.jumpAt+g.beforeMs+g.gapMs {
		return 0, false
	}
	return slots[0] - g.beforeMs, true
}

// FiresBetween is every fire of a cron strictly after from and at or
// before to, ascending, or false when there are more than limit: the fires
// NextFire gives one after another. It asks croner for fires in batches,
// far cheaper than one NextFire each, and drops any that do not move
// forward (the hour that repeats when clocks go back, see croneFireAfter).
// Around a spring-forward gap, where croner's batch can skip a moved slot
// or give one out of order, it counts with fireAfter instead.
func FiresBetween(p *Parsed, from, to int64, limit int) ([]int64, bool) {
	out := []int64{}
	start, ok := countFrom(from)
	if !ok {
		return out, true
	}
	loc := zoneOf(p)
	probe, last := start, start
	// step adds the fire after last, counted by fireAfter, and says whether
	// to count on; when not, over says whether out passed limit.
	step := func() (goOn, over bool) {
		t, ok := fireAfter(p, last)
		if !ok || t > to {
			return false, false
		}
		out = append(out, t)
		last, probe = t, t
		if len(out) > limit {
			return false, true
		}
		return true, false
	}
	for guard := 0; guard < 1000; guard++ {
		if _, in := springGap(loc, last); in {
			// Counting from inside a gap's window: croner would skip its moved slots.
			if goOn, over := step(); over {
				return nil, false
			} else if !goOn {
				return out, true
			}
			continue
		}
		batch := runsAfter(p.cron, min(limit+1-len(out), 24), probe)
		if len(batch) == 0 {
			return out, true
		}
		stepped := false
		for _, t := range batch {
			if t <= last {
				continue
			}
			if _, in := springGap(loc, t); in {
				// The batch reached a gap's window, where its order is
				// croner's: the next fire is counted from the last one, and
				// the rest with it.
				if goOn, over := step(); over {
					return nil, false
				} else if !goOn {
					return out, true
				}
				stepped = true
				break
			}
			if t > to {
				return out, true
			}
			out = append(out, t)
			last = t
			if len(out) > limit {
				return nil, false
			}
		}
		if stepped {
			continue
		}
		end := batch[len(batch)-1]
		if end > probe {
			probe = end
		} else {
			probe += 3_600_000
		}
	}
	return out, true
}

// NextFire is the next time the schedule fires strictly after from; for an
// interval, counted from the last run's start when there is one (see
// IntervalDue for a run that outlasted its period). False when a cron
// never fires again.
func NextFire(p *Parsed, from int64, lastRunAt *int64) (int64, bool) {
	if p.Kind == "interval" {
		base := from
		if lastRunAt != nil {
			base = *lastRunAt
		}
		return base + p.EveryMs, true
	}
	return fireAfter(p, from)
}

// IntervalDue is when an interval of everyMs is next due after a run: a
// period after it started, or, when it finished (finishedAt not nil) later
// than that, a period after it finished. A run that outlasts its period is
// not followed by a missed alert the moment it ends.
func IntervalDue(everyMs, startedAt int64, finishedAt *int64) int64 {
	due := startedAt + everyMs
	if finishedAt != nil && *finishedAt > due {
		return *finishedAt + everyMs
	}
	return due
}

// Expectation is when the next run is due, and when it is missed.
type Expectation struct {
	DueAt int64
	// Missed once now passes this.
	Deadline float64
}

// Expect is the SDK's expectation(): when the schedule next wants a run,
// given the last one. For a cron that is the first fire the last run does
// not already cover; with no run yet, the first fire at or after
// registration. For an interval it is the last run's start (or
// registration) plus the interval, or its finish plus the interval when it
// finished (lastFinishedAt, nil while it runs) later than that
// (IntervalDue). False for a cron that never fires again.
//
// Counting forward from the last run, rather than back from now, is what
// lets a job whose period is shorter than its grace be missed at all, and
// it works for a cron that fires once a year or less.
func Expect(p *Parsed, lastRunAt *int64, registeredAt int64, graceMs float64, lastFinishedAt *int64) (Expectation, bool) {
	var due int64
	var ok bool
	switch {
	case p.Kind == "interval" && lastRunAt == nil:
		due, ok = registeredAt+p.EveryMs, true
	case p.Kind == "interval":
		due, ok = IntervalDue(p.EveryMs, *lastRunAt, lastFinishedAt), true
	case lastRunAt == nil:
		due, ok = fireAfter(p, registeredAt-1)
	default:
		due, ok = dueAfterRun(p, *lastRunAt)
	}
	if !ok {
		return Expectation{}, false
	}
	return Expectation{DueAt: due, Deadline: float64(due) + graceMs}, true
}

// FireAfter is the first fire of a cron strictly after from, or false when
// it never fires again (see fireAfter). For the bridge package's check of a
// schedule converted from a scheduler's own.
func FireAfter(p *Parsed, from int64) (int64, bool) { return fireAfter(p, from) }

// DueAfterRun is the first fire of a cron that a run starting at startedAt
// does not cover, or false when there is none.
func DueAfterRun(p *Parsed, startedAt int64) (int64, bool) { return dueAfterRun(p, startedAt) }

// dueAfterRun is the first fire that a run starting at startedAt does not
// cover. A start before the year 1 covers none of them, so the first fire
// of the year 1 is due; after 9999 there is none (see countFrom).
func dueAfterRun(p *Parsed, startedAt int64) (int64, bool) {
	// A fire at or before the start is covered by the run itself.
	next, ok := fireAfter(p, startedAt)
	// Bounded: a gap holds at most a few hours of moved slots.
	for guard := 0; ok && guard < 100_000; guard++ {
		following, hasFollowing := fireAfter(p, next)
		var f *int64
		if hasFollowing {
			f = &following
		}
		if !RunCovers(startedAt, next, f) && !inSpringForwardGap(p, startedAt, next) {
			return next, true
		}
		next, ok = following, hasFollowing
	}
	return next, ok
}

// RunCovers is whether a run starting at startedAt covers the fire at
// dueAt. A minute of slack before the tick absorbs schedulers that fire a
// touch early. When the fire after dueAt is known, the slack is at most
// half the gap between the two, so one run of an every-minute cron never
// covers two fires.
func RunCovers(startedAt, dueAt int64, followingAt *int64) bool {
	slack := int64(EarlySlackMs)
	if followingAt != nil {
		slack = min(EarlySlackMs, js.FloorDiv(*followingAt-dueAt, 2))
	}
	return startedAt >= dueAt-slack
}

// inSpringForwardGap: on the night clocks spring forward, a slot whose
// local time does not exist (02:30 when 02:00 jumps to 03:00) fires moved
// forward by the gap (03:30; see gapFireAfter), while vixie cron runs every
// skipped job once, at the jump itself (03:00). A run that starts at or
// after the jump (less a minute of slack), and before such a moved fire, is
// taken to cover it, so neither scheduler's run is reported as missed. Only
// a moved slot that is not also a slot of its own on the later offset
// counts: a cron that fires every 10 minutes moves 02:10 onto its own
// 03:10, which was not skipped.
func inSpringForwardGap(p *Parsed, startedAt, fireAt int64) bool {
	g, ok := springGap(zoneOf(p), fireAt)
	if !ok || startedAt < g.jumpAt-EarlySlackMs || startedAt >= fireAt {
		return false
	}
	fires := func(at int64) bool {
		slots := runsAfter(p.wall, 1, at-1)
		return len(slots) > 0 && slots[0] == at
	}
	return fires(fireAt+g.beforeMs) && !fires(fireAt+g.beforeMs+g.gapMs)
}

func zoneOf(p *Parsed) *time.Location {
	loc, err := cron.LoadZone(p.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// utcOffset is the milliseconds the zone's wall clock is ahead of UTC at at.
func utcOffset(at int64, loc *time.Location) int64 {
	return cron.Offset(js.FloorDiv(at, 1000), loc) * 1000
}

// IsTimezone reports whether new Intl.DateTimeFormat("en-US", { timeZone })
// accepts the name: an IANA zone, matched without regard to case, "UTC",
// or a fixed offset such as "+05:30".
func IsTimezone(name string) bool {
	if name == "" {
		return false
	}
	_, err := cron.LoadZone(name)
	return err == nil
}

// LoadZone is the zone an IANA name names, matched without regard to case
// as Intl matches it; "" is the process's own zone, time.Local.
func LoadZone(name string) (*time.Location, error) {
	loc, err := cron.LoadZone(name)
	if err != nil {
		return nil, fmt.Errorf("timezone \"%s\" is not an IANA timezone", name)
	}
	return loc, nil
}
