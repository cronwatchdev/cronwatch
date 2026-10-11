defmodule Cronwatch.Conformance.OutboxTest do
  @moduledoc """
  Replays `conformance/client.json`'s `outbox` section: per-channel delivery
  through a real instance on a memory store, with fake channels that take or
  refuse each alert by their key (`Cronwatch.Evaluate.channel_keys/1`), and
  the stored state's `undelivered`, `sending`, and `last_alert_at` after each
  step, as the SDK's client leaves them.
  """
  use ExUnit.Case, async: true

  import Cronwatch.Test.Conformance, only: [field: 2, list: 2]

  alias Cronwatch.Evaluate
  alias Cronwatch.JobState
  alias Cronwatch.JS
  alias Cronwatch.JS.Object
  alias Cronwatch.Store
  alias Cronwatch.Test.Clock
  alias Cronwatch.Test.Conformance

  test "conformance/client.json: the outbox, retried per channel" do
    scenarios = list(Conformance.fixture("client"), "outbox")
    failures = Enum.flat_map(scenarios, &play/1)
    assert failures == [], "client.json outbox: #{length(failures)} steps differ:\n" <> Enum.join(failures, "\n")
    assert length(scenarios) == 8
  end

  defp play(scenario) do
    store_name = :"store#{System.unique_integer([:positive])}"
    start_supervised!({Cronwatch.Store.Memory, name: store_name}, id: store_name)
    clock = Clock.new()
    {:ok, down} = Agent.start_link(fn -> MapSet.new() end)
    {:ok, log} = Agent.start_link(fn -> [] end)
    {:ok, errors} = Agent.start_link(fn -> [] end)
    env = %{store: store_name, clock: clock, down: down, log: log, errors: errors}
    cw = make(env, list(scenario, "channels"))

    {failures, cw} =
      Enum.reduce(list(scenario, "steps"), {[], cw}, fn step, {failures, cw} ->
        cw = step(env, cw, step)
        got = snapshot(env, cw)
        want = field(step, "expect")
        label = "#{field(scenario, "name")}: #{field(step, "op")} #{JS.stringify(step)}"

        if JS.stringify(got) == JS.stringify(want),
          do: {failures, cw},
          else: {failures ++ ["#{label}\n  got  #{JS.stringify(got)}\n  want #{JS.stringify(want)}"], cw}
      end)

    stop_supervised(cw)
    failures
  end

  # An instance on the scenario's store with one fake channel per name.
  defp make(env, names) do
    name = :"cw#{System.unique_integer([:positive])}"
    keys = Evaluate.channel_keys(names)

    channels =
      names
      |> Enum.zip(keys)
      |> Enum.with_index()
      |> Enum.map(fn {{channel, key}, i} ->
        Cronwatch.Alerts.fun(channel, fn alert ->
          accepted = not Agent.get(env.down, &MapSet.member?(&1, key))
          run = alert.run

          entry =
            Object.new([
              {"channel", key},
              {"type", alert.type},
              {"run", run && run.id},
              {"accepted", accepted},
              {"errorLength", if(run && is_binary(run.error), do: JS.len16(run.error))},
              {"outputLength", if(run && is_binary(run.output), do: JS.len16(run.output))}
            ])

          Agent.update(env.log, &(&1 ++ [{i, entry}]))
          if accepted, do: :ok, else: {:error, "#{key} down"}
        end)
      end)

    start_supervised!(
      {Cronwatch,
       name: name,
       store: {Cronwatch.Store.Memory, server: env.store},
       clock: Clock.fun(env.clock),
       alerts: channels,
       cron_secret: false,
       on_error: fn _e, where -> Agent.update(env.errors, &(&1 ++ [where])) end},
      id: name
    )

    name
  end

  defp step(env, cw, step) do
    case field(step, "op") do
      op when op in ["fail", "ok"] ->
        Clock.set(env.clock, field(step, "at"))
        {:ok, h} = Cronwatch.start(Cronwatch.job!("nightly", instance: cw), id: field(step, "id"), instance: cw)
        Clock.set(env.clock, field(step, "at") + 1000)

        if op == "fail",
          do: Cronwatch.fail(h, error(field(step, "error"))),
          else: Cronwatch.finish(h, "done")

        cw

      "check" ->
        Clock.set(env.clock, field(step, "at"))
        Cronwatch.check!(instance: cw)
        cw

      "down" ->
        Agent.update(env.down, &MapSet.union(&1, MapSet.new(list(step, "channels"))))
        cw

      "up" ->
        Agent.update(env.down, &MapSet.difference(&1, MapSet.new(list(step, "channels"))))
        cw

      "configure" ->
        stop_supervised(cw)
        make(env, list(step, "channels"))

      # As an older release wrote it: no owed on any undelivered alert.
      "legacy" ->
        store = Cronwatch.Config.get(cw).store
        {:ok, %JobState{} = s} = Store.call(store, :get_state, ["nightly"])
        undelivered = Enum.map(s.undelivered, &%{&1 | extra: List.keydelete(&1.extra, "owed", 0)})
        :ok = Store.call(store, :set_state, [%{s | undelivered: undelivered}])
        cw
    end
  end

  defp error(nil), do: "boom"
  defp error(%Object{} = o), do: o |> Object.get("parts") |> Enum.map_join(fn [p, n] -> String.duplicate(p, n) end)
  defp error(text), do: text

  defp snapshot(env, cw) do
    {:ok, state} = Store.call(Cronwatch.Config.get(cw).store, :get_state, ["nightly"])
    state = state || %JobState{job: "nightly"}
    sent = env.log |> Agent.get_and_update(&{&1, []}) |> Enum.sort_by(&elem(&1, 0)) |> Enum.map(&elem(&1, 1))

    Object.new([
      {"sent", sent},
      {"undelivered", Enum.map(state.undelivered || [], &brief/1)},
      {"sending", for(%{alert: a} <- state.sending || [], do: brief(a))},
      {"lastAlertAt", state.last_alert_at},
      {"errors", Agent.get_and_update(env.errors, &{&1, []})}
    ])
  end

  defp brief(alert) do
    o = Object.new([{"type", alert.type}, {"run", alert.run && alert.run.id}])

    case List.keyfind(alert.extra || [], "owed", 0) do
      {_, owed} -> Object.put(o, "owed", owed)
      nil -> o
    end
  end
end
