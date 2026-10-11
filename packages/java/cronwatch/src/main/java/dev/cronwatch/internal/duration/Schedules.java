package dev.cronwatch.internal.duration;

import dev.cronwatch.internal.cron.Cron;
import dev.cronwatch.internal.cron.CronException;
import dev.cronwatch.internal.cron.Zones;
import dev.cronwatch.internal.js.Js;
import java.time.ZoneId;
import java.time.ZoneOffset;
import java.util.ArrayList;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import org.jspecify.annotations.Nullable;

/**
 * The SDK's {@code schedule.ts}: schedules ("0 2 * * *", "@hourly", "every 5m") with their fire
 * times, due times, deadlines, and what a run covers. Cron fire times come from the port of croner
 * in {@code internal.cron}, so a Java process and a Node, Ruby, Python, PHP, Go, Rust, or Elixir
 * process sharing one store agree on every due time. Refusals are {@link IllegalArgumentException}s
 * with the SDK's message.
 */
public final class Schedules {
  private Schedules() {}

  /** How early a run may start and still count for the fire it was meant for. */
  public static final long EARLY_SLACK_MS = 60_000;

  /**
   * The longest interval kept, 2^53 ms: added to any time the SDK meets it stays well inside a
   * {@code long}. A longer "every" is read as this long, which fires no sooner in any run's
   * lifetime.
   */
  static final double MAX_INTERVAL_MS = 9_007_199_254_740_992.0;

  /**
   * A schedule as {@code parseSchedule} returns it: a cron (with the zone it is read in, when one
   * was given) or an interval (with its period in milliseconds; 0 for a cron).
   */
  public record ParsedSchedule(
      Kind kind, String source, @Nullable String timezone, double everyMs) {
    /** Whether this is "every duration" rather than a cron. */
    public boolean isInterval() {
      return kind == Kind.INTERVAL;
    }
  }

  /** What kind of schedule a {@link ParsedSchedule} is. */
  public enum Kind {
    CRON,
    INTERVAL
  }

  /** When the next run is due, and when it is missed. */
  public record Expectation(long dueAt, double deadline) {}

  private static final int CACHE_LIMIT = 10_000;
  private static final Map<String, ParsedSchedule> CACHE = new ConcurrentHashMap<>();

  /** The croner expression behind each cron schedule, kept out of the public record. */
  private static final Map<ParsedSchedule, Cron> CRONS = new ConcurrentHashMap<>();

  /**
   * {@code parseSchedule}: a cron of five or six fields, a nickname, or "every 5m". Each (schedule,
   * timezone) pair is parsed once and kept. Without a timezone (null or "") a cron is read in the
   * JVM's zone, as crontab reads the system's.
   *
   * @throws IllegalArgumentException with the SDK's message
   */
  public static ParsedSchedule parse(String schedule, @Nullable String timezone) {
    String zone = timezone == null ? "" : timezone;
    String key = zone + "|" + schedule;
    ParsedSchedule hit = CACHE.get(key);
    if (hit != null) {
      return hit;
    }
    ParsedSchedule parsed = parseUncached(schedule, zone);
    // A long-running process parses a handful of schedules; the bound only keeps one fed endless
    // distinct schedules from growing without end.
    if (CACHE.size() >= CACHE_LIMIT) {
      CACHE.clear();
      CRONS.clear();
      WALLS.clear();
    }
    CACHE.put(key, parsed);
    return parsed;
  }

  private static ParsedSchedule parseUncached(String schedule, String zone) {
    String text = Js.trim(schedule);
    String rest = every(text);
    if (rest != null) {
      double ms = Durations.parse(rest, "schedule interval");
      if (ms < 1000) {
        throw new IllegalArgumentException(
            "schedule \"" + schedule + "\" is shorter than one second");
      }
      return new ParsedSchedule(Kind.INTERVAL, text, null, Math.min(ms, MAX_INTERVAL_MS));
    }
    ZoneId tz = zone.isEmpty() ? ZoneId.systemDefault() : Zones.find(zone);
    Cron cron;
    try {
      cron = Cron.parse(text, tz == null ? ZoneOffset.UTC : tz);
    } catch (CronException e) {
      throw new IllegalArgumentException(
          "schedule \""
              + schedule
              + "\" is not a cron expression or \"every <duration>\": "
              + e.getMessage(),
          e);
    }
    if (tz == null) {
      throw new IllegalArgumentException(
          "CronDate: Failed to convert date to timezone '"
              + zone
              + "'. This may happen with invalid timezone names or dates. Original error: toTZ:"
              + " Invalid timezone '"
              + zone
              + "' or date. Please provide a valid IANA timezone (e.g., 'America/New_York',"
              + " 'Europe/Stockholm'). Original error: Invalid time zone specified: "
              + zone);
    }
    ParsedSchedule parsed = new ParsedSchedule(Kind.CRON, text, zone.isEmpty() ? null : zone, 0);
    CRONS.put(parsed, cron);
    return parsed;
  }

  /** The croner expression behind a cron schedule, read again if it was let go. */
  private static Cron cronOf(ParsedSchedule p) {
    Cron cron = CRONS.get(p);
    if (cron != null) {
      return cron;
    }
    ParsedSchedule again = parseUncached(p.source(), p.timezone() == null ? "" : p.timezone());
    Cron made = CRONS.get(again);
    if (made == null || again.kind() != Kind.CRON) {
      throw new IllegalArgumentException(
          "schedule \"" + p.source() + "\" was not made by parseSchedule");
    }
    return made;
  }

  /**
   * {@code /^every\s+(.+)$/i}: "every" in any ASCII case, whitespace, and the rest, which must hold
   * no line terminator (JavaScript's "." matches none). Null when it does not match.
   */
  private static @Nullable String every(String text) {
    if (text.length() < 5 || !text.substring(0, 5).toLowerCase(Locale.ROOT).equals("every")) {
      return null;
    }
    for (int i = 0; i < 5; i++) {
      if (text.charAt(i) > 0x7f) {
        return null;
      }
    }
    int i = 5;
    while (i < text.length() && Js.isSpace(text.charAt(i))) {
      i++;
    }
    if (i == 5 || i == text.length()) {
      return null;
    }
    String rest = text.substring(i);
    for (int k = 0; k < rest.length(); k++) {
      char c = rest.charAt(k);
      if (c == '\n' || c == '\r' || c == ' ' || c == ' ') {
        return null;
      }
    }
    return rest;
  }

  /**
   * Four hundred Gregorian years: 146,097 days, a whole number of weeks, after which the calendar
   * repeats date for date and weekday for weekday.
   */
  private static final long CYCLE_MS = 146_097L * 86_400_000L;

  /** 0400-01-01T00:00:00Z: croner misreads a year below 100, so an earlier time is asked later. */
  private static final long CRONER_FIRST_MS = -49_544_438_400_000L;

  /** 2800-01-01T00:00:00Z: croner finds no fire past 3000, so a later time is asked earlier. */
  private static final long CRONER_LAST_MS = 26_192_246_400_000L;

  /**
   * {@code runsAfter}: the next {@code n} fires of a cron strictly after {@code from}, which lies
   * within the years 1 to 9999, dropping any after 9999. A time croner cannot answer for is moved
   * by whole 400-year cycles into the years it can, and its fires moved back: a time before 400
   * goes forward, into the same local mean time every zone kept then, and one from 2800 goes back,
   * to where the zone's present rules already hold.
   */
  private static List<Long> runsAfter(Cron cron, int n, long from) {
    long shift = 0;
    if (from < CRONER_FIRST_MS) {
      shift = Math.ceilDiv(CRONER_FIRST_MS - from, CYCLE_MS) * CYCLE_MS;
    } else if (from >= CRONER_LAST_MS) {
      shift = -((from - CRONER_LAST_MS) / CYCLE_MS + 1) * CYCLE_MS;
    }
    List<Long> out = new ArrayList<>(n);
    for (long t : cron.nextRuns(n, from + shift)) {
      long back = t - shift;
      if (back > Js.LAST_DATE_MS) {
        break;
      }
      out.add(back);
    }
    return out;
  }

  /**
   * {@code countFrom}: a stored time as a cron's fires are counted from it. A start read from a
   * foreign or damaged row can be any number: one before the year 1 counts from just before its
   * first millisecond, so the first fire of the year 1 is the next one, and one at or after the
   * last millisecond of 9999 has no fire after it at all (null). No fire is ever after 9999.
   */
  private static @Nullable Long countFrom(long from) {
    if (from >= Js.LAST_DATE_MS) {
      return null;
    }
    return Math.max(from, Js.FIRST_DATE_MS - 1);
  }

  /**
   * The first fire strictly after {@code from}, or null when the cron never fires again. On the
   * night clocks spring forward every slot the pattern names still fires: one whose local time the
   * jump skips is moved forward by the gap's length (see {@link #springGap} and {@link
   * #gapFireAfter}), so the fires stay strictly ascending whichever instant they are counted from.
   */
  private static @Nullable Long fireAfter(ParsedSchedule parsed, long from) {
    Cron cron = cronOf(parsed);
    Long found = croneFireAfter(cron, from);
    Long start = countFrom(from);
    if (start == null) {
      return found;
    }
    Long moved = gapFireAfter(parsed, cron, start);
    return moved != null && (found == null || moved < found) ? moved : found;
  }

  /**
   * The first fire strictly after {@code from} as croner counts it, or null when the cron never
   * fires again. croner answers with times in the past when asked from inside the hour that repeats
   * when clocks go back, so its answers are filtered, and a stretch of nothing but past times is
   * stepped over an hour at a time. Asked from inside a spring-forward gap it skips the slots moved
   * out of it, which {@link #fireAfter} adds back.
   */
  private static @Nullable Long croneFireAfter(Cron cron, long from) {
    Long start = countFrom(from);
    if (start == null) {
      return null;
    }
    long probe = start;
    for (int attempt = 0; attempt < 4; attempt++) {
      List<Long> runs = runsAfter(cron, 8, probe);
      if (runs.isEmpty()) {
        return null;
      }
      for (long t : runs) {
        if (t > start) {
          return t;
        }
      }
      probe += 3_600_000L;
    }
    return null;
  }

  /**
   * The window a spring-forward jump moves skipped slots into: from the jump ({@code jumpAt}, the
   * first instant on the later offset) for the gap's length ({@code gapMs}, the later offset less
   * the earlier one, so 30 minutes on Lord Howe Island and an hour in most zones). {@code beforeMs}
   * is the earlier offset.
   */
  private record Gap(long jumpAt, long gapMs, long beforeMs) {}

  /** How far back a spring-forward jump is looked for: more than any gap a zone has had. */
  private static final long JUMP_LOOKBACK_MS = 3 * 3_600_000L;

  /**
   * {@code springGap}: the window a spring-forward jump moves skipped slots into, when {@code at}
   * lies in one. Null when {@code at} is not within a gap's length after a jump in the three hours
   * before it.
   */
  private static @Nullable Gap springGap(Cron cron, long at) {
    Gap jump = springJump(cron, at);
    return jump != null && at < jump.jumpAt() + jump.gapMs() ? jump : null;
  }

  /**
   * The spring-forward jump in the three hours up to {@code at}, found to the minute: the first
   * minute on the later offset. Null when the offset at {@code at} is not ahead of the offset three
   * hours before.
   */
  private static @Nullable Gap springJump(Cron cron, long at) {
    // Every zone kept its local mean time, with no clock change, in the year 1.
    if (at - JUMP_LOOKBACK_MS < Js.FIRST_DATE_MS || at >= Js.LAST_DATE_MS) {
      return null;
    }
    long after = utcOffset(at, cron);
    long before = utcOffset(at - JUMP_LOOKBACK_MS, cron);
    if (after <= before) {
      return null;
    }
    long lo = at - JUMP_LOOKBACK_MS;
    long hi = at;
    while (hi - lo > 60_000) {
      long mid = lo + (hi - lo) / 2;
      if (utcOffset(mid, cron) == after) {
        hi = mid;
      } else {
        lo = mid;
      }
    }
    return new Gap(Math.floorDiv(hi, 60_000L) * 60_000L, after - before, before);
  }

  /** The pattern read in UTC, which stands for wall-clock time in {@link #gapFireAfter}. */
  private static final Map<ParsedSchedule, Cron> WALLS = new ConcurrentHashMap<>();

  private static Cron wallCron(ParsedSchedule parsed) {
    Cron wall = WALLS.get(parsed);
    if (wall == null) {
      if (WALLS.size() >= CACHE_LIMIT) {
        WALLS.clear();
      }
      wall = Cron.parse(parsed.source(), ZoneOffset.UTC);
      WALLS.put(parsed, wall);
    }
    return wall;
  }

  /**
   * The first slot moved out of a spring-forward gap strictly after {@code start}, when {@code
   * start} lies in that gap's window, else null. A slot whose local time the jump skips fires at
   * that local time read on the earlier offset, which is the same as moving it forward by the gap's
   * length: 01:20 on the night London skips 01:00 to 02:00 fires at 02:20 BST (01:20Z). croner
   * moves them the same way when asked from before the jump, but not from inside it.
   */
  private static @Nullable Long gapFireAfter(ParsedSchedule parsed, Cron cron, long start) {
    Gap gap = springGap(cron, start);
    if (gap == null) {
      return null;
    }
    // Wall-clock time as if it were UTC: the slots the jump skips run from jumpAt + beforeMs for
    // gapMs.
    List<Long> slots = runsAfter(wallCron(parsed), 1, start + gap.beforeMs());
    if (slots.isEmpty() || slots.get(0) >= gap.jumpAt() + gap.beforeMs() + gap.gapMs()) {
      return null;
    }
    return slots.get(0) - gap.beforeMs();
  }

  /**
   * {@code firesBetween}: every fire of a cron strictly after {@code from} and at or before {@code
   * to}, ascending, or null when there are more than {@code limit}: the fires {@link #nextFire}
   * gives one after another. Fires are asked for in batches, and any that do not move forward are
   * dropped (the hour that repeats when clocks go back). Around a spring-forward gap, where
   * croner's batch can skip a moved slot or give one out of order, it counts with {@link
   * #fireAfter} instead.
   */
  public static @Nullable List<Long> firesBetween(
      ParsedSchedule parsed, long from, long to, int limit) {
    Cron cron = cronOf(parsed);
    List<Long> out = new ArrayList<>();
    Long start = countFrom(from);
    if (start == null) {
      return out;
    }
    long probe = start;
    long last = start;
    for (int guard = 0; guard < 1000; guard++) {
      if (springGap(cron, last) != null) {
        // Counting from inside a gap's window: croner would skip its moved slots.
        Long t = fireAfter(parsed, last);
        if (t == null || t > to) {
          return out;
        }
        out.add(t);
        last = probe = t;
        if (out.size() > limit) {
          return null;
        }
        continue;
      }
      List<Long> batch = runsAfter(cron, Math.min(limit + 1 - out.size(), 24), probe);
      if (batch.isEmpty()) {
        return out;
      }
      boolean stepped = false;
      for (long t : batch) {
        if (t <= last) {
          continue;
        }
        if (springGap(cron, t) != null) {
          // The batch reached a gap's window, where its order is croner's: the next fire is
          // counted from the last one, and the rest with it.
          Long next = fireAfter(parsed, last);
          if (next == null || next > to) {
            return out;
          }
          out.add(next);
          last = probe = next;
          if (out.size() > limit) {
            return null;
          }
          stepped = true;
          break;
        }
        if (t > to) {
          return out;
        }
        out.add(t);
        last = t;
        if (out.size() > limit) {
          return null;
        }
      }
      if (stepped) {
        continue;
      }
      long end = batch.get(batch.size() - 1);
      probe = end > probe ? end : probe + 3_600_000L;
    }
    return out;
  }

  private static long add(long a, double ms) {
    long b = (long) ms;
    long sum = a + b;
    if (((a ^ sum) & (b ^ sum)) < 0) {
      return a < 0 ? Long.MIN_VALUE : Long.MAX_VALUE;
    }
    return sum;
  }

  /**
   * {@code nextFire}: the next time the schedule fires strictly after {@code from}; for an
   * interval, counted from the last run's start when there is one (see {@link #intervalDue} for a
   * run that outlasted its period). Null when a cron never fires again.
   */
  public static @Nullable Long nextFire(
      ParsedSchedule parsed, long from, @Nullable Long lastRunAt) {
    if (parsed.isInterval()) {
      return add(lastRunAt != null ? lastRunAt : from, parsed.everyMs());
    }
    return fireAfter(parsed, from);
  }

  /**
   * {@code intervalDue}: when an interval of {@code everyMs} is next due after a run: a period
   * after it started, or, when it finished ({@code finishedAt} not null) later than that, a period
   * after it finished. A run that outlasts its period is not followed by a missed alert the moment
   * it ends.
   */
  public static long intervalDue(double everyMs, long startedAt, @Nullable Long finishedAt) {
    long due = add(startedAt, everyMs);
    return finishedAt != null && finishedAt > due ? add(finishedAt, everyMs) : due;
  }

  /** {@link #expectation(ParsedSchedule, Long, long, double, Long)} with no finish known. */
  public static @Nullable Expectation expectation(
      ParsedSchedule parsed, @Nullable Long lastRunAt, long registeredAt, double graceMs) {
    return expectation(parsed, lastRunAt, registeredAt, graceMs, null);
  }

  /**
   * {@code expectation}: when the schedule next wants a run, given the last one. For a cron that is
   * the first fire the last run does not already cover; with no run yet, the first fire at or after
   * registration. For an interval it is the last run's start (or registration) plus the interval,
   * or its finish plus the interval when it finished ({@code lastFinishedAt}, null while it runs)
   * later than that ({@link #intervalDue}). Null for a cron that never fires again.
   */
  public static @Nullable Expectation expectation(
      ParsedSchedule parsed,
      @Nullable Long lastRunAt,
      long registeredAt,
      double graceMs,
      @Nullable Long lastFinishedAt) {
    Long due;
    if (parsed.isInterval()) {
      due =
          lastRunAt == null
              ? add(registeredAt, parsed.everyMs())
              : intervalDue(parsed.everyMs(), lastRunAt, lastFinishedAt);
    } else if (lastRunAt == null) {
      due = fireAfter(parsed, registeredAt == Long.MIN_VALUE ? registeredAt : registeredAt - 1);
    } else {
      due = dueAfterRun(parsed, lastRunAt);
    }
    return due == null ? null : new Expectation(due, (double) due + graceMs);
  }

  /**
   * The first fire that a run starting at {@code startedAt} does not cover. A start before the year
   * 1 covers none of them, so the first fire of the year 1 is due; after 9999 there is none.
   */
  private static @Nullable Long dueAfterRun(ParsedSchedule parsed, long startedAt) {
    Cron cron = cronOf(parsed);
    // A fire at or before the start is covered by the run itself.
    Long next = fireAfter(parsed, startedAt);
    // Bounded: a gap holds at most a few hours of moved slots.
    for (int guard = 0; next != null && guard < 100_000; guard++) {
      Long following = fireAfter(parsed, next);
      boolean covers =
          runCovers(startedAt, next, following)
              || inSpringForwardGap(parsed, cron, startedAt, next);
      if (!covers) {
        return next;
      }
      next = following;
    }
    return next;
  }

  /**
   * {@code runCovers}: whether a run starting at {@code startedAt} covers the fire at {@code
   * dueAt}. A minute of slack before the tick absorbs schedulers that fire a touch early. When the
   * fire after {@code dueAt} is known, the slack is at most half the gap between the two, so one
   * run of an every-minute cron never covers two fires.
   */
  public static boolean runCovers(long startedAt, long dueAt, @Nullable Long followingAt) {
    long slack =
        followingAt == null
            ? EARLY_SLACK_MS
            : Math.min(EARLY_SLACK_MS, Math.floorDiv(followingAt - dueAt, 2));
    return startedAt >= dueAt - slack;
  }

  /**
   * On the night clocks spring forward, a slot whose local time does not exist (02:30 when 02:00
   * jumps to 03:00) fires moved forward by the gap (03:30; see {@link #gapFireAfter}), while vixie
   * cron runs every skipped job once, at the jump itself (03:00). A run that starts at or after the
   * jump (less a minute of slack), and before such a moved fire, is taken to cover it, so neither
   * scheduler's run is reported as missed. Only a moved slot that is not also a slot of its own on
   * the later offset counts: a cron that fires every 10 minutes moves 02:10 onto its own 03:10,
   * which was not skipped.
   */
  private static boolean inSpringForwardGap(
      ParsedSchedule parsed, Cron cron, long startedAt, long fireAt) {
    Gap gap = springGap(cron, fireAt);
    if (gap == null || startedAt < gap.jumpAt() - EARLY_SLACK_MS || startedAt >= fireAt) {
      return false;
    }
    Cron wall = wallCron(parsed);
    return wallFires(wall, fireAt + gap.beforeMs())
        && !wallFires(wall, fireAt + gap.beforeMs() + gap.gapMs());
  }

  /** Whether the pattern read in UTC fires at exactly {@code at}. */
  private static boolean wallFires(Cron wall, long at) {
    List<Long> next = runsAfter(wall, 1, at - 1);
    return !next.isEmpty() && next.get(0) == at;
  }

  /** The milliseconds the zone's wall clock is ahead of UTC at {@code at}. */
  private static long utcOffset(long at, Cron cron) {
    return Zones.offset(Math.floorDiv(at, 1000), cron.rules()) * 1000;
  }

  /**
   * Whether {@code new Intl.DateTimeFormat("en-US", { timeZone })} accepts the name: an IANA zone,
   * matched without regard to case, or a fixed offset such as "+05:30".
   */
  public static boolean isZone(String name) {
    return !name.isEmpty() && Zones.find(name) != null;
  }
}
