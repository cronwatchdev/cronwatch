defmodule Cronwatch.Run.Exec do
  @moduledoc false
  # Runs a job's function as a recorded run (client.ts execute()).
  #
  # The function runs in the calling process by default, where the app's
  # context lives. Everything after it returns runs in a task of the
  # instance that the caller waits on, so a caller killed while it waits
  # leaves the recording to finish. A process that dies inside the function
  # has its run recorded by Cronwatch.Runs, which monitors it.

  alias Cronwatch.Config
  alias Cronwatch.Context
  alias Cronwatch.Core
  alias Cronwatch.Evaluate
  alias Cronwatch.Job
  alias Cronwatch.Lines
  alias Cronwatch.Output
  alias Cronwatch.Run
  alias Cronwatch.Runs
  alias Cronwatch.Store
  alias Cronwatch.Telemetry

  require Logger

  @doc "Runs `fun` as a recorded run of `job` and hands back what it did."
  def run(%Job{} = job, fun, opts) do
    case prepare(job, opts) do
      {:ok, c, prepared} -> run(c, job, fun, opts, prepared)
      :unavailable -> unrecorded(job.instance, job.name, fun, opts)
    end
  end

  # Everything before the function: the running row and the run's lines.
  # An instance stopping or restarting meanwhile runs the function
  # unrecorded rather than stop the job.
  defp prepare(job, opts) do
    c = Config.get(job.instance)
    trigger = Keyword.get(opts, :trigger, "run")
    started = start(c, job, trigger, nil, Keyword.get(opts, :discard_when) != nil)
    Lines.open(Runs.table(c.name, :lines), elem(started, 0).id)
    {:ok, c, started}
  rescue
    ArgumentError -> :unavailable
  catch
    :exit, _ -> :unavailable
  end

  defp run(c, job, fun, opts, {run, recorded, started, late}) do
    trigger = Keyword.get(opts, :trigger, "run")
    isolate = Keyword.get(opts, :isolate, false)
    kill = Keyword.get(opts, :kill_at_timeout, false)
    discard = Keyword.get(opts, :discard_when)
    {:ok, timeout} = Evaluate.timeout_ms(job.definition)
    lines = Runs.table(c.name, :lines)
    cancel = :atomics.new(1, [])

    ctx = %Context{
      instance: c.name,
      job: job.name,
      run_id: run.id,
      started_at: run.started_at,
      key: run.id,
      cancel: cancel,
      trigger: trigger
    }

    info = %{job: job, run: run, recorded: recorded, started: started, cancel: cancel, key: run.id, late: late}
    meta = %{instance: c.name, job: job.name, run: run.id, trigger: trigger}

    outcome =
      Telemetry.run_span(meta, fn ->
        outcome =
          if isolate,
            do: call_isolated(c, ctx, info, fun, timeout, kill, &settle(c, job, info, &1, discard, opts)),
            else: call_here(c, ctx, info, fun, timeout)

        {outcome, Map.put(meta, :status, status_of(outcome))}
      end)

    case outcome do
      {:recorded_elsewhere, reason} ->
        exit(reason)

      {:timed_out, _} ->
        c
        |> record(job, info, fn run ->
          snap = Lines.snapshot(lines, run.id)

          %{
            run
            | status: "timeout",
              metrics: snap.metrics,
              output: Core.clean(c, Lines.output(snap)),
              error: Core.clean(c, "Still running after #{Cronwatch.Duration.format(timeout)}; marked as timed out")
          }
        end)
        |> recorded(opts)

        exit(:timeout)

      _ ->
        settle(c, job, info, outcome, discard, opts)
        hand_back(outcome, isolate)
    end
  end

  @doc """
  Whether the instance can record a run: its config is in place and
  Cronwatch.Runs is up, its tables made.
  """
  def available?(instance) do
    :persistent_term.get({Cronwatch, instance}, nil) != nil and Runs.up?(instance)
  end

  @doc """
  Runs `fun` without recording it, for an instance that is not running (not
  started yet, stopping, or restarting after a crash): monitoring being down
  never stops the job. It is reported once per instance, through Logger. The
  function gets a context whose log and metric calls go nowhere, and what it
  answers, raises, throws, or exits comes back as `run/3` hands it back.
  """
  def unrecorded(instance, name, fun, opts) do
    warn_unavailable(instance, name)
    isolate = Keyword.get(opts, :isolate, false)

    ctx = %Context{
      instance: instance,
      job: name,
      run_id: Core.uuid(),
      started_at: System.system_time(:millisecond),
      key: {:unrecorded, make_ref()},
      cancel: :atomics.new(1, []),
      trigger: Keyword.get(opts, :trigger, "run")
    }

    outcome =
      if isolate do
        unrecorded_isolated(ctx, fun)
      else
        Context.push(ctx)

        try do
          invoke(fun, ctx)
        after
          Context.pop()
        end
      end

    hand_back(outcome, isolate)
  end

  # Isolated with no instance to run it under: a process of its own, so a
  # crash still does not take the caller down.
  defp unrecorded_isolated(ctx, fun) do
    callers = [self() | Process.get(:"$callers", [])]

    {_pid, ref} =
      spawn_monitor(fn ->
        Process.put(:"$callers", callers)
        Context.push(ctx)
        exit({:cronwatch_outcome, invoke(fun, ctx)})
      end)

    receive do
      {:DOWN, ^ref, :process, _, {:cronwatch_outcome, outcome}} -> outcome
      {:DOWN, ^ref, :process, _, reason} -> {:exit, reason, []}
    end
  end

  defp warn_unavailable(instance, name) do
    key = {Cronwatch, :unavailable_reported, instance}

    unless :persistent_term.get(key, false) do
      :persistent_term.put(key, true)

      Logger.warning(
        "[cronwatch] #{inspect(instance)} is not running, so #{name} ran unrecorded. " <>
          "Runs while the instance is down are not recorded; this is reported once."
      )
    end
  end

  # Takes the run back (discard_when) or records it as it ended.
  defp settle(c, job, info, outcome, discard, opts) do
    if discard && given_back?(c, job, discard, outcome) && take_back(c, job, info) do
      given_back(c, info, outcome_build(c, job, info, outcome), true)
    else
      c
      |> record_outcome(job, info, outcome)
      |> recorded(opts)
    end
  end

  # The run as its function ended: judged, recorded, and alerted on.
  defp record_outcome(c, job, info, outcome), do: record(c, job, info, outcome_build(c, job, info, outcome))

  # What makes the finished run of an outcome, from the run as it finished.
  defp outcome_build(c, job, info, outcome) do
    lines = Runs.table(c.name, :lines)
    failure = failure_text(outcome)
    result = result_of(outcome)

    fn run ->
      snap = Lines.snapshot(lines, info.key)
      text = returned_text(result)
      run = %{run | metrics: snap.metrics, output: Lines.output(snap) || text}
      Core.conclude(c, job.expect, run, failure, Lines.expect_text(snap) || text)
    end
  end

  # A run taken back lets go of its lines. One whose start write was given
  # up on and has not landed yet has no row to take back: that write is told
  # to take its row back when it lands, and handed the run as it ended
  # (`build`, judged or not as `evaluate` says), which it records in the
  # row's place when the store cannot take it back.
  defp given_back(c, info, build, evaluate) do
    lines = Runs.table(c.name, :lines)

    how =
      if not match?(%{late: %{}}, info) or landed?(info) do
        :used
      else
        try do
          finished_at = Core.now(c)
          duration = Evaluate.run_duration(info.run.started_at, finished_at)
          {:discarded, build.(%{info.run | finished_at: finished_at, duration_ms: duration}), finished_at, evaluate}
        rescue
          _ -> :discarded
        catch
          _, _ -> :discarded
        end
      end

    Lines.close(lines, info.key)
    conclude_late(info, how)
  end

  # Whether a failed run is one to take back (discard_when): the predicate
  # is asked about a returned {:error, reason} (or :error) and a raised
  # exception, never a throw or an exit. A predicate that raises is
  # reported and the run recorded.
  defp given_back?(c, job, discard, outcome) do
    reason =
      case outcome do
        {:returned, {:error, reason}} -> {:ok, reason}
        {:returned, :error} -> {:ok, :error}
        {:error, e, _} -> {:ok, e}
        _ -> :none
      end

    case reason do
      {:ok, reason} ->
        try do
          discard.(reason) not in [nil, false]
        rescue
          e ->
            Core.report(c, e, "discarding #{job.name}")
            false
        catch
          kind, value ->
            Core.report(c, {kind, value}, "discarding #{job.name}")
            false
        end

      :none ->
        false
    end
  end

  # Takes back a run still running, and says whether the caller is done with
  # it. A store without delete_run_if, or one that fails, is reported and the
  # run is recorded as it ended, so it is not left running to be reported
  # stuck; a row no longer running (a check marked it stuck meanwhile) is
  # reported and left as it is. A run whose start was never written has
  # nothing to take back (one whose write is still under way is taken back
  # by that write, when it lands: conclude_late/2).
  defp take_back(c, job, info, unjudged \\ false) do
    written = info.recorded or landed?(info)

    cond do
      not written and not match?(%{late: %{}}, info) ->
        true

      not Store.has?(c.store, :delete_run_if, 3) ->
        how = if unjudged, do: "recorded as it ended, not judged", else: "recorded as it ended"

        Core.report(
          c,
          Cronwatch.Error.other("the store cannot take back a run (it has no delete_run_if/4); #{how}"),
          "discarding #{job.name}"
        )

        false

      # Its start write is still under way: the row is taken back when it
      # lands (conclude_late/2).
      not written ->
        true

      true ->
        try do
          unless Core.store!(c, :delete_run_if, [info.run.id, job.name, "running"]) do
            Core.report(
              c,
              Cronwatch.Error.other("run #{info.run.id} of #{job.name} is no longer running; left as it is"),
              "discarding #{job.name}"
            )
          end

          true
        rescue
          e ->
            Core.report(c, e, "discarding #{job.name}")
            false
        end
    end
  end

  ## A run opened and closed by the caller (the scheduler integrations)

  @doc """
  Opens a run in the calling process, for an integration that sees a run's
  start and end as two events (a telemetry handler): the running row, the
  monitor on this process, the context for `current/0`, and the Logger
  metadata, as `run/3` sets them up. `close/2` or `take_back/2` must follow
  in the same process. Options: `trigger`, `id` (a run id of the
  integration's own), and `defer` (close missed only once the run
  is known not to be given back).
  """
  def open(%Job{} = job, opts) do
    c = Config.get(job.instance)
    trigger = Keyword.get(opts, :trigger, "run")
    {:ok, timeout} = Evaluate.timeout_ms(job.definition)

    {run, recorded, started, late} =
      start(c, job, trigger, Keyword.get(opts, :id), Keyword.get(opts, :defer, false))

    Lines.open(Runs.table(c.name, :lines), run.id)
    cancel = :atomics.new(1, [])

    ctx = %Context{
      instance: c.name,
      job: job.name,
      run_id: run.id,
      started_at: run.started_at,
      key: run.id,
      cancel: cancel,
      trigger: trigger
    }

    info = %{job: job, run: run, recorded: recorded, started: started, cancel: cancel, key: run.id, late: late}
    # Which Cronwatch.Runs holds the run: one restarted meanwhile does not.
    runs = GenServer.whereis(Runs.server(c.name))
    Runs.register(c.name, run.id, self(), info, timeout)
    Context.push(ctx)
    previous = Logger.metadata()
    Logger.metadata(cronwatch_job: job.name, cronwatch_run: run.id)
    meta = %{instance: c.name, job: job.name, run: run.id, trigger: trigger, telemetry_span_context: make_ref()}
    start_time = System.monotonic_time()

    :telemetry.execute(
      [:cronwatch, :run, :start],
      %{monotonic_time: start_time, system_time: System.system_time()},
      meta
    )

    %{config: c, job: job, info: info, previous: previous, meta: meta, start_time: start_time, runs: runs}
  end

  @doc """
  Closes a run `open/2` opened, with its outcome as `run/3` sees one
  (`{:returned, value}`, `{:error, exception, stacktrace}`, `{:throw, value,
  stacktrace}`, or `{:exit, reason, stacktrace}`), and records it. Nothing is
  recorded when the monitor already has (the process died meanwhile); a run
  lost with `Cronwatch.Runs`'s tables when it restarted is recorded here.
  """
  def close(%{config: c, job: job, info: info} = state, outcome) do
    if unwind(state, status_of(outcome)), do: record_outcome(c, job, info, outcome)
    :ok
  end

  @doc """
  Takes back a run `open/2` opened (an attempt given back without failing),
  as `discard_when` does; when the store cannot, the run is recorded with
  `outcome`, as it ended. With `outcome` `:unjudged` (a queue's snooze,
  nothing to judge), such a run is recorded as `ok` and not judged: the
  job's state is left as it was, so it neither recovers the job nor counts
  as a failure.
  """
  def take_back(%{config: c, job: job, info: info} = state, outcome) do
    if unwind(state, "discarded") do
      cond do
        take_back(c, job, info, outcome == :unjudged) ->
          if outcome == :unjudged,
            do: given_back(c, info, unjudged_build(c, info), false),
            else: given_back(c, info, outcome_build(c, job, info, outcome), true)

        outcome == :unjudged ->
          record_unjudged(c, job, info)

        true ->
          record_outcome(c, job, info, outcome)
      end
    end

    :ok
  end

  # A run given back that the store could not take back: stored as it
  # ended, ok, with its lines, and not judged.
  defp record_unjudged(c, job, info), do: record(c, job, info, unjudged_build(c, info), false)

  defp unjudged_build(c, info) do
    lines = Runs.table(c.name, :lines)

    fn run ->
      snap = Lines.snapshot(lines, info.key)
      Core.conclude(c, nil, %{run | metrics: snap.metrics, output: Lines.output(snap)}, nil, nil)
    end
  end

  # Undoes what open/2 set up in this process, and answers whether the run
  # is this process's to record: it was still open (the monitor did not
  # record it), or it went with Cronwatch.Runs's tables when that restarted
  # (or is restarting) since the run was opened, and no one else will.
  defp unwind(state, status) do
    Logger.reset_metadata(state.previous)
    Context.pop()
    now = System.monotonic_time()

    :telemetry.execute(
      [:cronwatch, :run, :stop],
      %{duration: now - state.start_time, monotonic_time: now},
      Map.put(state.meta, :status, status)
    )

    Runs.close(state.config.name, state.info.run.id) or
      case GenServer.whereis(Runs.server(state.config.name)) do
        nil -> true
        runs -> runs != Map.get(state, :runs)
      end
  end

  # The run as it was recorded, to the caller's own `recorded:` function
  # (Cronwatch.Handler answers with it), before the outcome is handed back.
  defp recorded(run, opts) do
    case Keyword.get(opts, :recorded) do
      f when is_function(f, 1) -> f.(run)
      _ -> :ok
    end
  end

  # How long run/3 (and a run an integration opens) waits for the store to
  # record a run's start, the job's declaration and the run's row, before
  # running the job without it. A store that fails does so sooner; this is
  # for one that hangs. The tests shorten it through the application
  # environment (:start_deadline), which apps leave alone.
  @start_deadline 5_000

  defp start_deadline, do: Application.get_env(:cronwatch, :start_deadline, @start_deadline)

  # The two flags a start write given up on shares with the run: the run
  # gave up waiting, and the write landed.
  @gave_up 1
  @landed 2

  # Inserts the running row and closes missed beside the job, which never
  # waits on it, or, for a run that may be given back, when it is recorded.
  # The store failing never stops the job, and one that hangs holds it for
  # start_deadline/0 at most. Answers `{run, recorded, started, late}`:
  # `late` is nil, or the write that was given up on (write_start/3).
  defp start(c, job, trigger, id, defer) do
    run = %Run{id: id || Core.uuid(), job: job.name, status: "running", started_at: Core.now(c), trigger: trigger}
    {recorded, late} = write_start(c, job, run)

    started =
      cond do
        not recorded ->
          nil

        defer ->
          :deferred

        true ->
          start_state(c, job, run)
      end

    {run, recorded, started, late}
  end

  # Writes the job's declaration and the running row, in a process of the
  # instance's task supervisor that the caller waits on for start_deadline/0,
  # and answers `{recorded, late}`.
  #
  # A store that hangs must not hold the job back: past the deadline the job
  # runs as one whose start was never written, which is reported. The write
  # is still under way and may land later, leaving a running row, so its
  # process stays to see the run through (`late`, kept in the run's info):
  # a write that lands before the finish is finished over as usual
  # (landed?/1); one that lands after the finish is told how the run ended
  # (conclude_late/2) and finishes it then, so no row is left running to be
  # called stuck; one that fails in the end leaves nothing behind.
  #
  # The caller monitors the process with an alias and is never linked to
  # it: giving up takes the monitor and the alias down together, so neither
  # the answer nor a :DOWN reaches its mailbox afterwards.
  defp write_start(c, job, run) do
    write = fn ->
      Core.sync!(c, job, true)
      Core.store!(c, :insert_run, [run])
    end

    flags = :atomics.new(2, [])

    case start_writer(c, fn -> writer(c, job, run, write, flags) end) do
      nil -> {written(c, job, guarded(write)), nil}
      pid -> await_start(c, job, pid, flags)
    end
  end

  defp start_writer(c, fun) do
    case Task.Supervisor.start_child(Cronwatch.Supervisor.tasks(c.name), fun) do
      {:ok, pid} -> pid
      _ -> nil
    end
  catch
    # The instance's task supervisor is restarting: written here, in the caller.
    :exit, _ -> nil
  end

  defp guarded(write) do
    write.()
    :ok
  rescue
    e -> {:error, e}
  catch
    kind, reason -> {:raised, kind, reason}
  end

  defp await_start(c, job, pid, flags) do
    ref = Process.monitor(pid, alias: :reply_demonitor)
    send(pid, {:cronwatch_start, ref})
    late = %{flags: flags, writer: pid}

    receive do
      {^ref, result} ->
        Process.demonitor(ref, [:flush])
        {written(c, job, result), nil}

      {:DOWN, ^ref, :process, _, reason} ->
        {written(
           c,
           job,
           {:error, Cronwatch.Error.other("the start was not written: #{Exception.format_exit(reason)}")}
         ), nil}
    after
      start_deadline() ->
        :atomics.put(flags, @gave_up, 1)
        Process.demonitor(ref, [:flush])

        # An answer that arrived as the deadline passed is still the answer.
        receive do
          {^ref, result} -> {written(c, job, result), late}
        after
          0 ->
            Core.report(
              c,
              Cronwatch.Error.other(
                "the store took longer than #{Cronwatch.Duration.format(start_deadline())} to record the start of " <>
                  "#{job.name}; running it unrecorded"
              ),
              "recording #{job.name}"
            )

            {false, late}
        end
    end
  end

  # Whether the start was written; a store's failure is reported, and an
  # exit or a throw in it is the caller's, as it was when the caller wrote.
  defp written(_c, _job, :ok), do: true

  defp written(c, job, {:error, e}) do
    Core.report(c, e, "recording #{job.name}")
    false
  end

  defp written(_c, _job, {:raised, kind, reason}), do: :erlang.raise(kind, reason, [])

  # The process that writes a run's start. Once the write has landed and
  # the run has given up on it, it waits to hear how the run ended.
  defp writer(c, job, run, write, flags) do
    receive do
      {:cronwatch_start, reply} ->
        result = guarded(write)
        if result == :ok, do: :atomics.put(flags, @landed, 1)
        send(reply, {reply, result})
        if result == :ok and :atomics.get(flags, @gave_up) == 1, do: finish_late(c, job, run)
    after
      # The caller ended before it asked.
      start_deadline() -> :ok
    end
  end

  defp finish_late(c, job, run) do
    receive do
      {:cronwatch_concluded, :used} ->
        :ok

      # The finish did not know of the row: it is finished and judged now.
      # A finish that was recorded all the same (the row found on its
      # insert) is found finished here, and nothing more happens.
      {:cronwatch_concluded, {:finish, finished, finished_at, evaluate}} ->
        Core.record_finish!(c, job, finished, true, finished_at, evaluate)

      # The run was given back: the row goes, or, from a store that cannot
      # take it back, is finished as the run ended.
      {:cronwatch_concluded, {:discarded, finished, finished_at, evaluate}} ->
        unless delete_late(c, job, run), do: Core.record_finish!(c, job, finished, true, finished_at, evaluate)

      # The run never came to a finish (its recording failed before one was made).
      {:cronwatch_concluded, :discarded} ->
        delete_late(c, job, run)
    end
  rescue
    e -> Core.report(c, e, "recording #{job.name}")
  catch
    kind, reason -> Core.report(c, {kind, reason}, "recording #{job.name}")
  end

  defp delete_late(c, job, run) do
    Store.has?(c.store, :delete_run_if, 3) and Core.store!(c, :delete_run_if, [run.id, job.name, "running"]) == true
  end

  # Whether a start write given up on has landed since.
  defp landed?(%{late: %{flags: flags}}), do: :atomics.get(flags, @landed) == 1
  defp landed?(_info), do: false

  # Tells a start write given up on how its run ended: `:used` (the finish
  # was written over its row, or the row was taken back), `{:finish, run,
  # finished_at, evaluate}` (the finish did not know of the row),
  # `{:discarded, run, finished_at, evaluate}` (given back before the row
  # was there), or `:discarded`.
  defp conclude_late(%{late: %{writer: pid}}, how), do: send(pid, {:cronwatch_concluded, how})
  defp conclude_late(_info, _how), do: :ok

  # Closes missed beside the job, in a task of the instance; nil
  # (nothing to wait for) when its task supervisor is restarting.
  defp start_state(c, job, run) do
    {:ok, pid} =
      Task.Supervisor.start_child(Cronwatch.Supervisor.tasks(c.name), fn ->
        try do
          Core.update_state!(c, job.name, fn before ->
            {Evaluate.start_run(job.definition, run, before, Core.now(c)), nil}
          end)
        rescue
          e -> Core.report(c, e, "starting #{job.name}")
        end
      end)

    pid
  catch
    :exit, _ -> nil
  end

  defp call_here(c, ctx, info, fun, timeout) do
    Runs.register(c.name, ctx.run_id, self(), info, timeout)
    Context.push(ctx)
    previous = Logger.metadata()
    Logger.metadata(cronwatch_job: ctx.job, cronwatch_run: ctx.run_id)

    try do
      invoke(fun, ctx)
    after
      Logger.reset_metadata(previous)
      Context.pop()
      Runs.close(c.name, ctx.run_id)
    end
  end

  # In a task of the instance, for a function that should not take its
  # caller down. With kill_at_timeout, the task is killed at the job's
  # timeout and the run recorded as a check would mark it.
  #
  # Whoever closes the run in Cronwatch.Runs records it: the task, when its
  # function returns, hands the outcome to the caller and waits for the
  # caller to take it, and records it itself when the caller is gone (killed
  # while it waited), so the run is never left running; the caller, at the
  # timeout or when the task dies; or the monitor, when the task is killed.
  defp call_isolated(c, ctx, info, fun, timeout, kill, orphan) do
    parent = self()
    id = ctx.run_id

    work = fn ->
      # Which Cronwatch.Runs holds the run: one restarted meanwhile does not.
      runs = GenServer.whereis(Runs.server(c.name))
      Runs.register(c.name, id, self(), info, timeout)
      send(parent, {:cronwatch_registered, id, runs})
      Context.push(ctx)
      Logger.metadata(cronwatch_job: ctx.job, cronwatch_run: id)
      outcome = invoke(fun, ctx)
      # Closed here, before the task ends, so the monitor never takes a
      # normal end for a death. A run no longer open was closed by the
      # caller at the timeout, which does not wait for an answer, or lost
      # with Cronwatch.Runs's tables when it restarted: the outcome goes
      # back as the task's answer, for the caller to record.
      if Runs.close(c.name, id) do
        hand_over(parent, id, outcome, orphan)
        :ok
      else
        {:lost, outcome}
      end
    end

    case start_isolated(c, work) do
      {:ok, task} ->
        task_ref = task.ref

        receive do
          {:cronwatch_registered, ^id, runs} ->
            deadline = if kill, do: now_ms() + max(Cronwatch.JS.to_int(timeout), 0)
            await_isolated(c, task, {id, runs}, deadline, timeout)

          # Gone before it was registered: no one else knows of the run.
          {:DOWN, ^task_ref, :process, _, reason} ->
            {:exit, reason, []}
        end

      # The instance's task supervisor is restarting: the function runs in
      # the caller, still recorded.
      :unavailable ->
        call_here(c, ctx, info, fun, timeout)
    end
  end

  defp start_isolated(c, work) do
    {:ok, Task.Supervisor.async_nolink(Cronwatch.Supervisor.tasks(c.name), work)}
  catch
    :exit, _ -> :unavailable
  end

  # The longest a receive can wait at once.
  @max_wait 4_294_967_295

  defp now_ms, do: System.monotonic_time(:millisecond)

  defp await_isolated(c, task, {id, runs} = held, deadline, timeout) do
    wait = if deadline, do: min(max(deadline - now_ms(), 0), @max_wait), else: :infinity
    task_ref = task.ref

    receive do
      {:cronwatch_outcome, ^id, pid, outcome} ->
        send(pid, {:cronwatch_taken, id})
        Task.yield(task, :infinity)
        outcome

      # The run was lost with Cronwatch.Runs's tables (it restarted while the
      # function ran): no one else will record it.
      {^task_ref, {:lost, outcome}} ->
        # The task ends as it answers: its :DOWN follows, and is taken too.
        receive do
          {:DOWN, ^task_ref, :process, _, _} -> outcome
        end

      # Closed here, the run is the caller's to record. Closed elsewhere, the
      # monitor recorded it, unless it went with Cronwatch.Runs when that
      # restarted (taking the instance's tasks, this one too, down with it):
      # then no one else will.
      {:DOWN, ^task_ref, :process, _, reason} ->
        cond do
          Runs.close(c.name, id) -> {:exit, reason, []}
          GenServer.whereis(Runs.server(c.name)) != runs -> {:exit, reason, []}
          true -> {:recorded_elsewhere, reason}
        end
    after
      wait ->
        cond do
          deadline - now_ms() > 0 ->
            await_isolated(c, task, held, deadline, timeout)

          # Past the timeout: closed first, so the monitor does not record
          # the kill as a failure.
          Runs.close(c.name, id) ->
            Task.shutdown(task, :brutal_kill)
            {:timed_out, timeout}

          # The function returned meanwhile: its outcome is on its way.
          true ->
            await_isolated(c, task, held, nil, timeout)
        end
    end
  end

  # The task's side: the caller records the outcome once it has taken it; a
  # caller that died first leaves the recording to the task.
  defp hand_over(parent, id, outcome, orphan) do
    ref = Process.monitor(parent)
    send(parent, {:cronwatch_outcome, id, self(), outcome})

    receive do
      {:cronwatch_taken, ^id} -> Process.demonitor(ref, [:flush])
      {:DOWN, ^ref, :process, _, _} -> orphan.(outcome)
    end
  end

  defp invoke(fun, ctx) do
    {:returned, fun.(ctx)}
  rescue
    e -> {:error, e, __STACKTRACE__}
  catch
    :throw, value -> {:throw, value, __STACKTRACE__}
    :exit, reason -> {:exit, reason, __STACKTRACE__}
  end

  # Everything after the function: in a task that outlives the caller.
  defp record(c, job, info, build, evaluate \\ true) do
    case start_isolated(c, fn -> record_now(c, job, info, build, evaluate) end) do
      {:ok, task} ->
        # nil when the recording itself failed, so a caller's recorded:
        # function is never handed something that is not a run.
        case Task.yield(task, :infinity) do
          {:ok, run} ->
            run

          {:exit, reason} ->
            Core.report(c, {:exit, reason}, "recording #{job.name}")
            nil
        end

      # The instance's task supervisor is restarting: recorded here, in the
      # caller.
      :unavailable ->
        record_now(c, job, info, build, evaluate)
    end
  end

  defp record_now(c, job, info, build, evaluate) do
    {run, how} = finish_now(c, job, info, build, evaluate)
    conclude_late(info, how)
    run
  end

  # Answers `{run, how}`: the run as recorded (nil when the recording itself
  # failed), and what a start write given up on is told (conclude_late/2).
  defp finish_now(c, job, info, build, evaluate) do
    lines = Runs.table(c.name, :lines)

    try do
      finished_at = Core.now(c)

      run = %{
        info.run
        | finished_at: finished_at,
          duration_ms: Evaluate.run_duration(info.run.started_at, finished_at)
      }

      run = build.(run)
      Lines.close(lines, info.key)
      # A run not judged does not judge its start either.
      unless not evaluate and info.started == :deferred, do: wait_for(c, job, info.started, info.run)
      # A start write given up on that has landed since is a row to finish over.
      recorded = info.recorded or landed?(info)
      how = if recorded, do: :used, else: {:finish, run, finished_at, evaluate}

      try do
        case Core.record_finish!(c, job, run, recorded, finished_at, evaluate) do
          nil ->
            :ok

          why ->
            Core.report(
              c,
              Cronwatch.Error.other("run #{run.id} of #{job.name} #{why}; ignored"),
              "finishing #{job.name}"
            )
        end

        {run, how}
      rescue
        e ->
          Core.report(c, e, "recording #{job.name}")
          {run, how}
      catch
        kind, reason ->
          Core.report(c, {kind, reason}, "recording #{job.name}")
          {nil, how}
      end
    rescue
      e ->
        Core.report(c, e, "recording #{job.name}")
        {nil, :discarded}
    catch
      kind, reason ->
        Core.report(c, {kind, reason}, "recording #{job.name}")
        {nil, :discarded}
    after
      # Whatever happened (an app's clock that raised), the run's lines
      # do not outlive it.
      Lines.close(lines, info.key)
    end
  end

  defp wait_for(_c, _job, nil, _run), do: :ok

  # A run that might have been given back closes missed now that
  # it is known not to be.
  defp wait_for(c, job, :deferred, run) do
    Core.update_state!(c, job.name, fn before -> {Evaluate.start_run(job.definition, run, before, Core.now(c)), nil} end)
  rescue
    e -> Core.report(c, e, "starting #{job.name}")
  end

  defp wait_for(_c, _job, pid, _run) do
    ref = Process.monitor(pid)

    receive do
      {:DOWN, ^ref, :process, _, _} -> :ok
    end
  end

  @doc "Records a run whose process died inside the function, as failed, with the exit reason."
  def record_dead(instance, _id, info, reason) do
    c = Config.get(instance)

    record(c, info.job, info, fn run ->
      snap = Lines.snapshot(Runs.table(c.name, :lines), run.id)
      run = %{run | metrics: snap.metrics, output: Lines.output(snap)}
      Core.conclude(c, nil, run, Output.describe_uncapped(:exit, reason, []), nil)
    end)
  end

  # What counts as failed: a raise, a throw, an exit, {:error, reason} and
  # :error; and an HTTP answer of 400 or more.
  defp failure_text({:error, e, st}), do: Output.describe_uncapped(:error, e, st)
  defp failure_text({:throw, v, st}), do: Output.describe_uncapped(:throw, v, st)
  defp failure_text({:exit, r, st}), do: Output.describe_uncapped(:exit, r, st)
  defp failure_text({:returned, {:error, reason}}), do: Output.describe_uncapped(:returned, reason, [])
  defp failure_text({:returned, :error}), do: Output.describe_uncapped(:returned, :error, [])
  defp failure_text({:returned, value}), do: http_failure(value)

  defp status_of({:returned, {:error, _}}), do: "failed"
  defp status_of({:returned, :error}), do: "failed"
  defp status_of({:returned, value}), do: if(http_failure(value), do: "failed", else: "ok")
  defp status_of({:timed_out, _}), do: "timeout"
  defp status_of(_), do: "failed"

  @doc false
  # A Plug.Conn, or a Req.Response alone or in {:ok, _}, of status 400 or
  # more, as the SDK fails a run for a fetch Response.
  def http_failure({:ok, value}), do: http_failure(value)

  def http_failure(%{__struct__: struct, status: status})
      when struct in [Plug.Conn, Req.Response] and is_integer(status) do
    if status >= 400, do: "HTTP #{status}" <> reason_phrase(status)
  end

  def http_failure(_), do: nil

  defp reason_phrase(status) do
    if Code.ensure_loaded?(Plug.Conn.Status) do
      # Plug is optional, so the call is made at run time, once it is loaded.
      # credo:disable-for-next-line Credo.Check.Refactor.Apply
      " " <> apply(Plug.Conn.Status, :reason_phrase, [status])
    else
      ""
    end
  rescue
    _ -> ""
  end

  defp result_of({:returned, value}), do: value
  defp result_of(_), do: nil

  # A binary the function returns, or {:ok, binary}, is the output when
  # nothing was logged, and what expect checks.
  defp returned_text(text) when is_binary(text), do: text
  defp returned_text({:ok, text}) when is_binary(text), do: text
  defp returned_text(_), do: nil

  # A raise, throw, or exit is raised again with its stacktrace, so the
  # caller's own handling sees exactly what it would have without
  # CronWatch; a returned value is returned. An isolated run's crash is
  # handed back as an exit.
  defp hand_back({:returned, value}, _isolate), do: value
  defp hand_back({:error, e, st}, _isolate), do: :erlang.raise(:error, e, st)
  defp hand_back({:throw, v, st}, _isolate), do: :erlang.raise(:throw, v, st)
  defp hand_back({:exit, r, st}, false), do: :erlang.raise(:exit, r, st)
  defp hand_back({:exit, r, _st}, true), do: exit(r)
end
