defmodule Cronwatch.Schedule do
  @moduledoc false
  # Internal: not the package's API, and it can change in any release.
  #
  # The SDK's `schedule.ts`: schedules (`"0 2 * * *"`, `"@hourly"`,
  # `"every 5m"`) with their fire times, due times, deadlines, and what a run
  # covers. Cron fire times come from the croner port (`Cronwatch.Cron`), so an
  # Elixir process and a Node, Ruby, Python, PHP, Go, or Rust process sharing
  # one store agree on every due time.
  #
  # A schedule is read on each call; reading one is cheap next to the store
  # calls around it, and nothing is kept between calls.

  alias Cronwatch.Cron
  alias Cronwatch.Duration
  alias Cronwatch.JS
  alias Cronwatch.JS.Object
  alias Cronwatch.Zone

  @early_slack_ms 60_000
  # The first and last milliseconds written as dates: 0001-01-01T00:00:00.000Z and 9999-12-31T23:59:59.999Z.
  @first_date_ms -62_135_596_800_000
  @last_date_ms 253_402_300_799_999
  # Four hundred Gregorian years: 146,097 days, a whole number of weeks,
  # after which the calendar repeats date for date and weekday for weekday.
  @cycle_ms 146_097 * 86_400_000
  # Croner misreads a year below 100 and finds no fire past the year 3000, so
  # the SDK asks it about a time before the year 400 a cycle or more later,
  # and one from the year 2800 a cycle or more earlier; so does this port.
  @croner_first_ms -49_544_438_400_000
  @croner_last_ms 26_192_246_400_000
  @max_interval_ms 9_007_199_254_740_992
  @hour 3_600_000
  # How far back a spring-forward jump is looked for: more than any gap a zone has had.
  @jump_lookback_ms 3 * @hour

  defstruct [:kind, :source, :timezone, :every_ms, :cron]

  @typedoc """
  A schedule as `parseSchedule` returns it: `kind` is `"cron"` or
  `"interval"`, `timezone` the name given (nil when none), `every_ms` an
  interval's period, and `cron` the croner expression behind a cron.
  """
  @type t :: %__MODULE__{
          kind: String.t(),
          source: String.t(),
          timezone: String.t() | nil,
          every_ms: integer() | nil,
          cron: Cron.t() | nil
        }

  @typedoc "When the next run is due, and the time past which it is missed."
  @type expectation :: %{due_at: integer(), deadline: number()}

  @doc "How early a run may start and still count for the fire it was meant for: a minute."
  @spec early_slack_ms() :: 60_000
  def early_slack_ms, do: @early_slack_ms

  @doc """
  The longest interval kept, 2^53 ms. A longer `every` is read as this long,
  which fires no sooner in any run's lifetime.
  """
  @spec max_interval_ms() :: pos_integer()
  def max_interval_ms, do: @max_interval_ms

  @doc """
  The SDK's `parseSchedule`: a cron of five or six fields, a nickname
  (`"@hourly"`), or `"every 5m"`. Without a timezone (`nil` or `""`) a cron
  is read in the process's zone, as crontab reads the system's; Vercel and
  GitHub Actions run their crons in UTC, so pass `"UTC"` for those. The
  errors are the SDK's, word for word; a zone the database does not have is
  refused here with the message croner throws for it when asked for a fire
  time, since the SDK reads the zone only then (see `timezone?/1`).
  """
  @spec parse(String.t(), String.t() | nil) :: {:ok, t()} | {:error, String.t()}
  def parse(schedule, timezone \\ nil)
  def parse(schedule, ""), do: parse(schedule, nil)

  def parse(schedule, timezone) do
    text = JS.trim(schedule)

    case every(text) do
      {:ok, rest} -> interval(schedule, text, rest)
      :error -> cron(schedule, text, timezone)
    end
  end

  defp interval(schedule, text, rest) do
    with {:ok, ms} <- Duration.parse(rest, "schedule interval") do
      if ms < 1000 do
        {:error, "schedule \"#{schedule}\" is shorter than one second"}
      else
        {:ok, %__MODULE__{kind: "interval", source: text, every_ms: min(ms, @max_interval_ms)}}
      end
    end
  end

  defp cron(schedule, text, timezone) do
    pattern_zone = if timezone == nil, do: Zone.system(), else: :utc

    with {:ok, _} <- tag(Cron.new(text, pattern_zone), schedule),
         {:ok, zone} <- zone(timezone),
         {:ok, c} <- Cron.new(text, zone) do
      {:ok, %__MODULE__{kind: "cron", source: text, timezone: timezone, cron: c}}
    end
  end

  defp tag({:error, message}, schedule),
    do: {:error, "schedule \"#{schedule}\" is not a cron expression or \"every <duration>\": #{message}"}

  defp tag(ok, _schedule), do: ok

  defp zone(nil), do: {:ok, Zone.system()}

  defp zone(timezone) do
    case Zone.load(timezone) do
      {:ok, zone} ->
        {:ok, zone}

      {:error, _} ->
        {:error,
         "CronDate: Failed to convert date to timezone '#{timezone}'. This may happen with invalid timezone names or dates. " <>
           "Original error: toTZ: Invalid timezone '#{timezone}' or date. Please provide a valid IANA timezone (e.g., 'America/New_York', 'Europe/Stockholm'). " <>
           "Original error: Invalid time zone specified: #{timezone}"}
    end
  end

  # /^every\s+(.+)$/i: "every" in any ASCII case, whitespace, and the rest,
  # which must hold no line terminator (JavaScript's "." matches none).
  defp every(<<head::binary-size(5), rest::binary>>) do
    trimmed = JS.trim_start(rest)

    lower = for <<c <- head>>, into: "", do: <<if(c in ?A..?Z, do: c + 32, else: c)>>

    if lower == "every" and byte_size(trimmed) < byte_size(rest) and trimmed != "" and
         not String.contains?(trimmed, ["\n", "\r", "\u2028", "\u2029"]),
       do: {:ok, trimmed},
       else: :error
  end

  defp every(_), do: :error

  @doc """
  The SDK's JSON of a parsed schedule: `{kind, source, timezone}` for a cron
  (timezone only when given) and `{kind, source, everyMs}` for an interval.
  """
  @spec to_value(t()) :: Object.t()
  def to_value(%__MODULE__{} = p) do
    o = Object.new([{"kind", p.kind}, {"source", p.source}])

    cond do
      p.kind == "interval" -> Object.put(o, "everyMs", p.every_ms)
      p.timezone != nil -> Object.put(o, "timezone", p.timezone)
      true -> o
    end
  end

  @doc """
  Whether `new Intl.DateTimeFormat("en-US", { timeZone })` accepts the name:
  an IANA zone in any case, `"UTC"`, or a fixed offset such as `"+05:30"`.
  """
  @spec timezone?(term()) :: boolean()
  def timezone?(name), do: Zone.zone?(name)

  defp next_runs(%__MODULE__{cron: nil}, _count, _start), do: []
  defp next_runs(%__MODULE__{cron: c}, count, start), do: runs_after(c, count, start)

  # The SDK's runsAfter: the next `count` fires after `start`, which lies
  # within the years 1 to 9999, found where croner can answer (moved by
  # whole 400-year cycles) and moved back, dropping any after 9999.
  defp runs_after(c, count, start) do
    shift =
      cond do
        start < @croner_first_ms -> ceil_div(@croner_first_ms - start, @cycle_ms) * @cycle_ms
        start >= @croner_last_ms -> -(Integer.floor_div(start - @croner_last_ms, @cycle_ms) + 1) * @cycle_ms
        true -> 0
      end

    c
    |> Cron.next_runs(count, start + shift)
    |> Enum.map(&(&1 - shift))
    |> Enum.take_while(&(&1 <= @last_date_ms))
  end

  defp ceil_div(a, b), do: -Integer.floor_div(-a, b)

  # The SDK's countFrom: a stored time as a cron's fires are counted from it.
  # A start read from a foreign or damaged row can be any number: one before
  # the year 1 counts from just before its first millisecond, and one at or
  # after the last millisecond of 9999 has no fire after it (nil).
  defp count_from(from) when from >= @last_date_ms, do: nil
  defp count_from(from) when from >= @first_date_ms, do: from
  defp count_from(_from), do: @first_date_ms - 1

  @doc """
  The first fire strictly after `from`, or nil when the cron never fires
  again. On the night clocks spring forward every slot the pattern names
  still fires: one whose local time the jump skips is moved forward by the
  gap's length (see `spring_gap/2`), so the fires stay strictly ascending
  whichever instant they are counted from.
  """
  @spec fire_after(t(), integer()) :: integer() | nil
  def fire_after(p, from) do
    case count_from(from) do
      nil ->
        nil

      start ->
        found = crone_fire_after(p, start, start, 0)
        moved = gap_fire_after(p, start)
        if moved != nil and (found == nil or moved < found), do: moved, else: found
    end
  end

  # The first fire strictly after `from` as croner counts it. Croner answers
  # with times in the past when asked from inside the hour that repeats when
  # clocks go back, so its answers are filtered, and a stretch of nothing but
  # past times is stepped over an hour at a time. Asked from inside a
  # spring-forward gap it skips the slots moved out of it, which fire_after
  # adds back.
  defp crone_fire_after(_p, _from, _probe, 4), do: nil

  defp crone_fire_after(p, from, probe, attempt) do
    case next_runs(p, 8, probe) do
      [] ->
        nil

      runs ->
        case Enum.find(runs, &(&1 > from)) do
          nil -> crone_fire_after(p, from, probe + @hour, attempt + 1)
          t -> t
        end
    end
  end

  @doc """
  The window a spring-forward jump moves skipped slots into, when `at` lies
  in one: `{jump_at, gap_ms, before_ms}`, from the jump (the first instant
  on the later offset) for the gap's length (the later offset less the
  earlier one, so 30 minutes on Lord Howe Island and an hour in most zones),
  with the earlier offset. Nil when `at` is not within a gap's length after
  a jump in the three hours before it, and for an interval.
  """
  @spec spring_gap(t(), integer()) :: {integer(), integer(), integer()} | nil
  def spring_gap(%__MODULE__{cron: nil}, _at), do: nil

  def spring_gap(%__MODULE__{cron: c}, at) do
    case spring_jump(c.zone, at) do
      {jump_at, gap_ms, _before_ms} = jump when at < jump_at + gap_ms -> jump
      _ -> nil
    end
  end

  # The spring-forward jump in the three hours up to `at`, found to the
  # minute: the first minute on the later offset. Nil when the offset at
  # `at` is not ahead of the offset three hours before.
  defp spring_jump(zone, at) do
    # Every zone kept its local mean time, with no clock change, in the year 1.
    if at - @jump_lookback_ms < @first_date_ms or at >= @last_date_ms do
      nil
    else
      after_offset = utc_offset(at, zone)
      before_offset = utc_offset(at - @jump_lookback_ms, zone)

      if after_offset > before_offset do
        hi = find_jump(at - @jump_lookback_ms, at, after_offset, zone)
        {Integer.floor_div(hi, 60_000) * 60_000, after_offset - before_offset, before_offset}
      end
    end
  end

  # The pattern read in UTC, which stands for wall-clock time around a gap.
  defp wall(c), do: %{c | zone: :utc}

  # The first slot moved out of a spring-forward gap strictly after `start`,
  # when `start` lies in that gap's window, else nil. A slot whose local time
  # the jump skips fires at that local time read on the earlier offset, which
  # is the same as moving it forward by the gap's length: 01:20 on the night
  # London skips 01:00 to 02:00 fires at 02:20 BST (01:20Z). Croner moves
  # them the same way when asked from before the jump, but not from inside it.
  defp gap_fire_after(%__MODULE__{cron: nil}, _start), do: nil

  defp gap_fire_after(%__MODULE__{cron: c} = p, start) do
    case spring_gap(p, start) do
      nil ->
        nil

      {jump_at, gap_ms, before_ms} ->
        # Wall-clock time as if it were UTC: the slots the jump skips run
        # from jump_at + before_ms for gap_ms.
        case runs_after(wall(c), 1, start + before_ms) do
          [slot] when slot < jump_at + before_ms + gap_ms -> slot - before_ms
          _ -> nil
        end
    end
  end

  @doc """
  Every fire of a cron strictly after `from` and at or before `to`,
  ascending, or nil when there are more than `limit`: the fires `next_fire/3`
  gives one after another. Fires are asked for in batches, and any that do
  not move forward are dropped (the hour that repeats when clocks go back).
  Around a spring-forward gap, where croner's batch can skip a moved slot
  or give one out of order, they are counted with `fire_after/2` instead.
  """
  @spec fires_between(t(), integer(), integer(), non_neg_integer()) :: [integer()] | nil
  def fires_between(p, from, to, limit) do
    case count_from(from) do
      nil -> []
      start -> between(p, to, limit, start, start, [], 0, 0)
    end
  end

  defp between(_p, _to, _limit, _probe, _last, out, _n, 1000), do: Enum.reverse(out)

  defp between(p, to, limit, probe, last, out, n, guard) do
    if spring_gap(p, last) do
      # Counting from inside a gap's window: croner would skip its moved slots.
      step(p, to, limit, last, out, n, guard)
    else
      case next_runs(p, min(limit + 1 - n, 24), probe) do
        [] ->
          Enum.reverse(out)

        batch ->
          case take(p, batch, to, limit, last, out, n) do
            {:done, result} ->
              result

            # The batch reached a gap's window, where its order is croner's:
            # the next fire is counted from the last one, and the rest with it.
            {:step, last, out, n} ->
              step(p, to, limit, last, out, n, guard)

            {:more, last, out, n} ->
              stop = List.last(batch)
              probe = if stop > probe, do: stop, else: probe + @hour
              between(p, to, limit, probe, last, out, n, guard + 1)
          end
      end
    end
  end

  defp step(p, to, limit, last, out, n, guard) do
    case fire_after(p, last) do
      nil -> Enum.reverse(out)
      t when t > to -> Enum.reverse(out)
      _ when n + 1 > limit -> nil
      t -> between(p, to, limit, t, t, [t | out], n + 1, guard + 1)
    end
  end

  defp take(_p, [], _to, _limit, last, out, n), do: {:more, last, out, n}
  defp take(p, [t | rest], to, limit, last, out, n) when t <= last, do: take(p, rest, to, limit, last, out, n)

  defp take(p, [t | rest], to, limit, last, out, n) do
    cond do
      spring_gap(p, t) -> {:step, last, out, n}
      t > to -> {:done, Enum.reverse(out)}
      n + 1 > limit -> {:done, nil}
      true -> take(p, rest, to, limit, t, [t | out], n + 1)
    end
  end

  @doc """
  The next time the schedule fires strictly after `from`; for an interval,
  counted from the last run's start when there is one (see `interval_due/3`
  for a run that outlasted its period). Nil when a cron never fires again.
  """
  @spec next_fire(t(), integer(), integer() | nil) :: integer() | nil
  def next_fire(%__MODULE__{kind: "interval", every_ms: every}, from, last_run_at), do: (last_run_at || from) + every
  def next_fire(p, from, _last_run_at), do: fire_after(p, from)

  @doc """
  When an interval of `every_ms` is next due after a run: a period after it
  started, or, when it finished (`finished_at` not nil) later than that, a
  period after it finished. A run that outlasts its period is not followed
  by a missed alert the moment it ends.
  """
  @spec interval_due(integer(), integer(), integer() | nil) :: integer()
  def interval_due(every_ms, started_at, finished_at) do
    due = started_at + every_ms
    if finished_at != nil and finished_at > due, do: finished_at + every_ms, else: due
  end

  @doc """
  The SDK's `expectation()`: when the schedule next wants a run, given the
  last one. For a cron that is the first fire the last run does not already
  cover; with no run yet, the first fire at or after registration. For an
  interval it is the last run's start (or registration) plus the interval,
  or its finish plus the interval when it finished (`last_finished_at`, nil
  while it runs) later than that (`interval_due/3`). Nil for a cron that
  never fires again.
  """
  @spec expectation(t(), integer() | nil, integer(), number(), integer() | nil) :: expectation() | nil
  def expectation(p, last_run_at, registered_at, grace_ms, last_finished_at \\ nil) do
    due =
      cond do
        p.kind == "interval" and last_run_at == nil -> registered_at + p.every_ms
        p.kind == "interval" -> interval_due(p.every_ms, last_run_at, last_finished_at)
        last_run_at == nil -> fire_after(p, registered_at - 1)
        true -> due_after_run(p, last_run_at)
      end

    if due, do: %{due_at: due, deadline: JS.normalize(due + grace_ms)}
  end

  @doc "The first fire of a cron that a run starting at `started_at` does not cover, or nil."
  @spec due_after_run(t(), integer()) :: integer() | nil
  def due_after_run(p, started_at) do
    # A fire at or before the start is covered by the run itself.
    p |> fire_after(started_at) |> uncovered(p, started_at, 0)
  end

  # Bounded: a gap holds at most a few hours of moved slots.
  defp uncovered(next, _p, _started_at, guard) when next == nil or guard >= 100_000, do: next

  defp uncovered(next, p, started_at, guard) do
    following = fire_after(p, next)

    if run_covers(started_at, next, following) or in_spring_forward_gap?(p, started_at, next),
      do: uncovered(following, p, started_at, guard + 1),
      else: next
  end

  @doc """
  Whether a run starting at `started_at` covers the fire at `due_at`. A
  minute of slack before the tick absorbs schedulers that fire a touch
  early. When the fire after `due_at` is known, the slack is at most half the
  gap between the two, so one run of an every-minute cron never covers two
  fires.
  """
  @spec run_covers(integer(), integer(), integer() | nil) :: boolean()
  def run_covers(started_at, due_at, following_at \\ nil) do
    slack =
      if following_at == nil,
        do: @early_slack_ms,
        else: min(@early_slack_ms, Integer.floor_div(following_at - due_at, 2))

    started_at >= due_at - slack
  end

  # On the night clocks spring forward, a slot whose local time does not
  # exist (02:30 when 02:00 jumps to 03:00) fires moved forward by the gap
  # (03:30; see gap_fire_after), while vixie cron runs every skipped job
  # once, at the jump itself (03:00). A run that starts at or after the jump
  # (less a minute of slack), and before such a moved fire, is taken to
  # cover it, so neither scheduler's run is reported as missed. Only a moved
  # slot that is not also a slot of its own on the later offset counts: a
  # cron that fires every 10 minutes moves 02:10 onto its own 03:10, which
  # was not skipped.
  defp in_spring_forward_gap?(%__MODULE__{cron: nil}, _started_at, _fire_at), do: false

  defp in_spring_forward_gap?(%__MODULE__{cron: c} = p, started_at, fire_at) do
    case spring_gap(p, fire_at) do
      {jump_at, gap_ms, before_ms} when started_at >= jump_at - @early_slack_ms and started_at < fire_at ->
        wall = wall(c)
        wall_fires?(wall, fire_at + before_ms) and not wall_fires?(wall, fire_at + before_ms + gap_ms)

      _ ->
        false
    end
  end

  defp wall_fires?(wall, at), do: runs_after(wall, 1, at - 1) == [at]

  # The first minute in the window with the later offset.
  defp find_jump(lo, hi, after_offset, zone) when hi - lo > 60_000 do
    mid = lo + div(hi - lo, 2)

    if utc_offset(mid, zone) == after_offset,
      do: find_jump(lo, mid, after_offset, zone),
      else: find_jump(mid, hi, after_offset, zone)
  end

  defp find_jump(_lo, hi, _after_offset, _zone), do: hi

  # The milliseconds the zone's wall clock is ahead of UTC at `at`.
  defp utc_offset(at, zone), do: Zone.offset(Integer.floor_div(at, 1000), zone) * 1000
end
