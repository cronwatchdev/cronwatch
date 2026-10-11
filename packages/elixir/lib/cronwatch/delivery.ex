defmodule Cronwatch.Delivery do
  @moduledoc false
  # Composing, triaging, sending, and queueing alerts (client.ts outbox,
  # dispatch, retryUndelivered, recordDelivery, deliver, and addTriage).

  alias Cronwatch.ChannelContext
  alias Cronwatch.Config
  alias Cronwatch.Core
  alias Cronwatch.Evaluate
  alias Cronwatch.Format
  alias Cronwatch.Telemetry

  # How long one channel may take to send one alert.
  @channel_timeout 15_000
  @triage_timeout 25_000
  # Wall-clock time one check spends retrying undelivered alerts, across
  # every job.
  @retry_budget 20_000

  # The three limits, read when used; the tests shorten them through the
  # application environment (:channel_timeout, :triage_timeout,
  # :retry_budget), which apps leave alone.
  defp limit(key, default), do: Application.get_env(:cronwatch, key, default)
  defp channel_timeout, do: limit(:channel_timeout, @channel_timeout)
  defp triage_timeout, do: limit(:triage_timeout, @triage_timeout)
  defp retry_budget, do: limit(:retry_budget, @retry_budget)

  @doc """
  An evaluation as it is written, inside `Cronwatch.Core.update_state!/3`
  (so it only computes): its drafts composed into alerts and held in the
  same state (`Cronwatch.Evaluate.hold_alerts/4`), so the write that opens a
  condition also keeps its alerts, and a process that stops before sending
  them does not lose them. Answers `{state, {alerts, dropped}}`.
  """
  def outbox(%Config{} = c, state, drafts, definition, now) do
    alerts = Enum.map(drafts, &Format.compose_alert(&1, definition, now))
    until = Core.now(c) + Evaluate.send_lease_ms()
    {state, dropped} = Evaluate.hold_alerts(state, alerts, until, c.deliver == :check)
    {state, {alerts, dropped}}
  end

  @doc "Reports alerts let go because a job's queue was full."
  def report_dropped(_c, _name, dropped) when dropped <= 0, do: :ok

  def report_dropped(c, name, dropped) do
    Core.report(
      c,
      Cronwatch.Error.other(
        "#{dropped} undelivered alert#{if dropped == 1, do: "", else: "s"} for #{name} dropped: " <>
          "only the newest #{Evaluate.max_undelivered()} are kept for retry"
      ),
      "alert queue for #{name}"
    )
  end

  @doc """
  Triages and sends each alert the outbox holds (see `outbox/5`). The state,
  with the alerts in it, was saved before this, so a slow channel holds up
  nothing else; afterwards only the delivery fields are written back, onto a
  fresh read of the state, and the alerts leave `sending`. Triage is made
  here, never stored with the held alert. With `deliver: :check` the alerts
  were queued for a check elsewhere instead. Answers the alerts.
  """
  def dispatch(_c, _name, [], _now), do: []

  def dispatch(%Config{deliver: :check} = c, _name, alerts, _now) do
    for alert <- alerts, do: Telemetry.alert(:queued, meta(c, alert, nil))
    alerts
  end

  def dispatch(%Config{} = c, name, alerts, now) do
    {sent, delivered, failed, reached} =
      Enum.reduce(alerts, {[], [], [], false}, fn alert, {sent, delivered, failed, reached} ->
        alert = if c.triage && alert.type != "recovered", do: add_triage(c, alert, triage_timeout()), else: alert

        # The alert as composed, full run and all; only a retry reads the
        # trimmed copy held in state.
        case deliver(c, alert) do
          {nil, took} ->
            {sent ++ [alert], delivered ++ [alert], failed, reached or took}

          {left, took} ->
            Telemetry.alert(:queued, meta(c, alert, nil))
            {sent ++ [alert], delivered, failed ++ [left], reached or took}
        end
      end)

    record_delivery(c, name, delivered, failed, [], now, reached)
    sent
  end

  @doc """
  Sends the alerts some channel refused last time, once each, oldest first,
  to the channels that still owe them (their `owed`) and no others, from the
  copy held in state, its run trimmed (`Cronwatch.Evaluate.held_run/1`).
  They are claimed first (`Cronwatch.Evaluate.claim_undelivered/3`): moved
  into `sending` with a lease, in one conditional write, so when several
  processes check at once only the one whose write lands sends them. One
  that no longer describes the job is dropped instead. Retries across a
  check share the retry budget of wall-clock time, kept in `spent`; claimed
  alerts it has no time left for go back to the queue for the next check.
  Answers `{sent, spent}`: the alerts some channel took on this attempt.
  """
  def retry_undelivered(%Config{} = c, name, state, now, spent) do
    if (state.undelivered || []) == [] or c.deliver == :check do
      {[], spent}
    else
      case claim(c, name, now) do
        [] -> {[], spent}
        claimed -> retry(c, name, claimed, now, spent)
      end
    end
  end

  defp claim(c, name, now) do
    {_, {claimed, stale}} =
      Core.update_state!(c, name, fn previous ->
        until = Core.now(c) + Evaluate.send_lease_ms()
        {state, claimed, stale} = Evaluate.claim_undelivered(Evaluate.normalize_state(previous, name), now, until)
        {state, {claimed, stale}}
      end)

    for a <- stale, do: Telemetry.alert(:dropped, meta(c, a, nil))
    claimed
  rescue
    e ->
      Core.report(c, e, "claiming undelivered alerts for #{name}")
      []
  end

  defp retry(c, name, claimed, now, spent) do
    {delivered, failed, sent, spent} =
      Enum.reduce(claimed, {[], [], [], spent}, fn alert, {delivered, failed, sent, spent} ->
        left = retry_budget() - spent

        if left <= 0 do
          # Not tried: back to the queue as it was, owed and all.
          {delivered, failed ++ [alert], sent, spent}
        else
          started = System.monotonic_time(:millisecond)

          # An alert queued by a process that delivers at check time was
          # never triaged. One that was tried is not tried again.
          alert =
            if c.triage && alert.type != "recovered" && not alert.triage_tried,
              do: add_triage(c, alert, min(triage_timeout(), left)),
              else: alert

          {left_owing, took} = deliver(c, alert)
          sent = if took, do: sent ++ [Evaluate.without_owed(alert)], else: sent

          {delivered, failed} =
            if left_owing, do: {delivered, failed ++ [left_owing]}, else: {delivered ++ [alert], failed}

          {delivered, failed, sent, spent + max(0, System.monotonic_time(:millisecond) - started)}
        end
      end)

    record_delivery(c, name, delivered, failed, [], now, sent != [])
    {sent, spent}
  end

  # Marks delivered alerts done, drops stale ones, and keeps failed ones for
  # the next check, taking them all out of `sending` (record_sent). A failed
  # alert replaces its stored copy, so a triage made on this attempt and the
  # channels that still owe it are kept. last_alert_at moves when `reached`,
  # some channel accepted an alert on this attempt. When this write fails,
  # alerts still in `sending` are retried once their lease runs out.
  defp record_delivery(c, name, delivered, failed, dropped, now, reached) do
    {_, trimmed} =
      Core.update_state!(c, name, fn previous ->
        Evaluate.record_sent(Evaluate.normalize_state(previous, name), delivered, failed, dropped, now, reached)
      end)

    report_dropped(c, name, trimmed)
  rescue
    e -> Core.report(c, e, "recording alert delivery for #{name}")
  end

  @doc """
  Sends to every channel that still owes the alert
  (`Cronwatch.Evaluate.owed_channels/2`: all of them for an alert not sent
  before), at once, each in a task of its own stopped after 15 seconds, each
  failure reported under its channel's name. Answers `{left, reached}`:
  `left` is the alert with `owed` cut to the channels that refused it, or
  nil when none did (or none is left to owe it), and `reached` whether any
  channel accepted it. With no channels at all it counts as delivered.
  """
  def deliver(%Config{alerts: []}, _alert), do: {nil, true}

  def deliver(%Config{} = c, alert) do
    named = Enum.map(c.alerts, &{&1, channel_name(&1)})
    keys = Evaluate.channel_keys(Enum.map(named, &elem(&1, 1)))

    case Evaluate.owed_channels(alert, keys) do
      [] -> {nil, false}
      owed -> send_owed(c, alert, Enum.zip(named, keys), owed)
    end
  end

  defp send_owed(c, alert, channels, owed) do
    plain = Evaluate.without_owed(alert)

    tasks =
      for {{{module, state}, name}, key} <- channels, key in owed do
        ctx = %ChannelContext{on_error: fn e -> Core.report(c, e, "alert channel #{name}") end, transport: c.transport}

        task =
          Task.Supervisor.async_nolink(Cronwatch.Supervisor.tasks(c.name), fn ->
            guarded(fn -> module.send(state, plain, ctx) end)
          end)

        {task, name, key}
      end

    results = Task.yield_many(Enum.map(tasks, &elem(&1, 0)), channel_timeout())

    failed =
      tasks
      |> Enum.zip(results)
      |> Enum.flat_map(fn {{task, name, key}, {_, result}} ->
        outcome =
          case result do
            {:ok, :ok} ->
              :ok

            {:ok, {:error, reason}} ->
              {:error, reason}

            # Described by its shape alone: an app's channel may answer with
            # its own state, credentials and all.
            {:ok, other} ->
              {:error, "channel #{name} answered #{Config.shape(other)}, not :ok or {:error, reason}"}

            {:exit, reason} ->
              {:error, {:exit, reason}}

            nil ->
              Task.shutdown(task, :brutal_kill)
              {:error, Cronwatch.Error.other("timed out after #{channel_timeout()}ms")}
          end

        case outcome do
          :ok ->
            Telemetry.alert(:sent, meta(c, alert, name))
            []

          {:error, reason} ->
            Core.report(c, reason, "alert channel #{name}")
            Telemetry.alert(:failed, meta(c, alert, name))
            [key]
        end
      end)

    {Evaluate.after_send(alert, owed, failed), length(failed) < length(owed)}
  end

  defp channel_name({module, state}) do
    module.name(state)
  rescue
    _ -> inspect(module)
  catch
    _, _ -> inspect(module)
  end

  # Sets the alert's triage to the diagnosis, or to nil when there is none, so
  # it is tried once per alert.
  defp add_triage(c, alert, timeout) do
    recent =
      try do
        Core.store!(c, :list_runs, [alert.job, 5])
      rescue
        _ -> []
      end

    context = %{alert: alert, recent_runs: recent, transport: c.transport}

    task =
      Task.Supervisor.async_nolink(Cronwatch.Supervisor.tasks(c.name), fn ->
        guarded(fn ->
          case c.triage do
            f when is_function(f, 1) -> f.(context)
            {module, opts} -> module.triage(opts, context)
            module when is_atom(module) -> module.triage([], context)
          end
        end)
      end)

    result =
      case Task.yield(task, timeout) || Task.shutdown(task, :brutal_kill) do
        {:ok, {:ok, text}} when is_binary(text) and text != "" -> {:ok, text}
        {:ok, text} when is_binary(text) and text != "" -> {:ok, text}
        {:ok, {:error, reason}} -> {:error, reason}
        {:ok, _} -> {:ok, nil}
        {:exit, reason} -> {:error, {:exit, reason}}
        nil -> {:error, Cronwatch.Error.other("timed out after #{timeout}ms")}
      end

    case result do
      {:ok, text} ->
        %{alert | triage: text, triage_tried: true}

      {:error, reason} ->
        Core.report(c, reason, "triage for #{alert.job}")
        %{alert | triage: nil, triage_tried: true}
    end
  end

  # A raise, throw, or exit in a channel or triage is its error, answered
  # from the task rather than crashing it (and logging a crash report).
  defp guarded(fun) do
    fun.()
  rescue
    e -> {:error, e}
  catch
    :exit, reason -> {:error, {:exit, reason}}
    :throw, value -> {:error, {:throw, value}}
  end

  defp meta(c, alert, channel) do
    %{
      instance: c.name,
      job: alert.job,
      type: alert.type,
      condition: if(alert.type == "recovered", do: nil, else: alert.type),
      channel: channel
    }
  end
end
