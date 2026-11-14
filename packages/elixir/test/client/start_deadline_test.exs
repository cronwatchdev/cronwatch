defmodule Cronwatch.StartDeadlineTest do
  @moduledoc """
  The deadline on a run's start (the SDK's START_DEADLINE_MS): a store that
  hangs while the job's declaration and the run's row are written holds the
  job for five seconds at most, and the write that lands late never leaves a
  running row behind. The SDK's tests (client.test.ts), with the deadline
  shortened through the application environment.
  """
  # Not async: the deadline is shortened for the package.
  use ExUnit.Case, async: false

  import Cronwatch.Test.Client

  alias Cronwatch.Test.Capture
  alias Cronwatch.Test.Stores

  @deadline 300

  setup do
    Application.put_env(:cronwatch, :start_deadline, @deadline)
    on_exit(fn -> Application.delete_env(:cronwatch, :start_deadline) end)
  end

  # An instance whose store's first insert_run (a run's start) hangs until
  # `release.()`; `later` is what the inserts after it do.
  defp hanging(later \\ fn _args, call -> call.() end) do
    {hooked, hooks} = Stores.hooked(Stores.memory())
    inserts = :counters.new(1, [])
    test = self()

    Stores.hook(hooks, :insert_run, fn args, call ->
      :counters.add(inserts, 1, 1)

      if :counters.get(inserts, 1) == 1 do
        send(test, {:hung, self()})

        receive do
          :release -> call.()
        end
      else
        later.(args, call)
      end
    end)

    Map.put(make(store: Stores.option(hooked)), :store, hooked)
  end

  defp release(writer), do: send(writer, :release)
  defp runs(cw, job), do: Cronwatch.runs!(job, 50, instance: cw)

  defp running(store) do
    {:ok, runs} = Cronwatch.Store.call(store, :running_runs, [])
    runs
  end

  defp took_too_long(job),
    do: "the store took longer than #{@deadline}ms to record the start of #{job}; running it unrecorded"

  test "the deadline is five seconds, written as the SDK writes it" do
    Application.delete_env(:cronwatch, :start_deadline)
    assert Cronwatch.Duration.format(5_000) == "5s"
  end

  test "a store that hangs while recording a run's start does not hold the job back, and leaves no running row" do
    %{cw: cw, errors: errors, alerts: alerts, store: store} = hanging()
    test = self()

    running =
      Task.async(fn ->
        Cronwatch.run(
          "nightly",
          fn j ->
            send(test, :ran)
            Cronwatch.log(j, "did the work")
            "done"
          end,
          instance: cw
        )
      end)

    assert_receive {:hung, writer}, 1000
    refute_receive :ran, div(@deadline, 2), "still waiting on the store"
    assert_receive :ran, 1000, "the job ran once the deadline passed"
    assert Task.await(running) == "done"
    assert messages(errors) == [took_too_long("nightly")]
    assert wheres(errors) == ["recording nightly"]

    # The finish was written without the start. The start write lands at
    # last, finds the run there, and nothing more happens.
    assert [%{status: "ok", output: "did the work"}] = runs(cw, "nightly")
    ref = Process.monitor(writer)
    release(writer)
    assert_receive {:DOWN, ^ref, :process, _, _}, 1000
    assert [%{status: "ok", output: "did the work"}] = runs(cw, "nightly")
    assert running(store) == []
    assert length(messages(errors)) == 1
    assert Capture.types(alerts) == []
  end

  test "a start write that lands before the finish is finished over as usual" do
    %{cw: cw, errors: errors, alerts: alerts, store: store} = hanging()
    test = self()

    running =
      Task.async(fn ->
        Cronwatch.run(
          "nightly",
          fn _ ->
            send(test, {:ran, self()})

            receive do
              :go -> {:error, "no space left"}
            end
          end,
          instance: cw
        )
      end)

    assert_receive {:hung, writer}, 1000
    assert_receive {:ran, job}, 1000
    # The late start lands while the job is still running.
    release(writer)
    eventually(fn -> match?([%{status: "running"}], runs(cw, "nightly")) end)
    send(job, :go)
    assert Task.await(running) == {:error, "no space left"}
    assert [%{status: "failed", error: "no space left"}] = runs(cw, "nightly")
    assert running(store) == []
    assert messages(errors) == [took_too_long("nightly")]
    assert Capture.types(alerts) == ["failed"]
    ref = Process.monitor(writer)
    assert_receive {:DOWN, ^ref, :process, _, _}, 1000
  end

  test "a start write that lands after the finish failed is finished then, not left running" do
    down = fn _args, _call -> {:error, %RuntimeError{message: "store down"}} end
    %{cw: cw, errors: errors, alerts: alerts, store: store} = hanging(down)

    running = Task.async(fn -> catch_error(Cronwatch.run("nightly", fn _ -> raise "boom" end, instance: cw)) end)
    assert_receive {:hung, writer}, 1000
    assert %RuntimeError{message: "boom"} = Task.await(running), "the job's own error is what run/3 raises"
    assert wheres(errors) == ["recording nightly", "recording nightly"]
    assert runs(cw, "nightly") == []

    # The start write lands at last, after the finish gave up: the run is
    # finished and judged now.
    release(writer)
    eventually(fn -> match?([%{status: "failed"}], runs(cw, "nightly")) end)
    assert [%{error: "RuntimeError: boom" <> _}] = runs(cw, "nightly")
    assert running(store) == []
    eventually(fn -> Capture.types(alerts) == ["failed"] end)
  end

  test "a start write that lands while the finish, written as a run of its own, is failing is finished over" do
    {hooked, hooks} = Stores.hooked(Stores.memory())
    inserts = :counters.new(1, [])
    test = self()

    Stores.hook(hooks, :insert_run, fn [run], call ->
      :counters.add(inserts, 1, 1)

      if :counters.get(inserts, 1) == 1 do
        send(test, {:hung, self()})

        receive do
          :release -> call.()
        end
      else
        # The finish, written as a run of its own since the start had not
        # landed. The start lands under it, and then the finish fails, its
        # read of what the store holds too.
        send(test, {:finishing, self(), run.id})

        receive do
          :fail -> {:error, %RuntimeError{message: "store down"}}
        end
      end
    end)

    %{cw: cw, errors: errors, alerts: alerts} = make(store: Stores.option(hooked))
    running = Task.async(fn -> Cronwatch.run("nightly", fn _ -> "done" end, instance: cw) end)
    assert_receive {:hung, writer}, 1000
    assert_receive {:finishing, finisher, id}, 2000
    release(writer)
    eventually(fn -> match?({:ok, %{status: "running"}}, Cronwatch.get_run(id, instance: cw)) end)

    Stores.hook(hooks, :get_run, fn _args, call ->
      if self() == finisher do
        Stores.unhook(hooks, :get_run)
        {:error, %RuntimeError{message: "store down"}}
      else
        call.()
      end
    end)

    send(finisher, :fail)
    assert Task.await(running) == "done"

    # The finish failed and left the row to no one; the start write, which
    # landed meanwhile, finishes the row over its own start.
    eventually(fn -> match?([%{status: "ok"}], runs(cw, "nightly")) end)
    assert running(hooked) == []
    assert wheres(errors) == ["recording nightly", "recording nightly"]
    assert Capture.types(alerts) == []
    ref = Process.monitor(writer)
    assert_receive {:DOWN, ^ref, :process, _, _}, 1000
  end

  test "a run given back whose late start write cannot be taken back is finished as it ended" do
    %{cw: cw, errors: errors, store: store} = hanging()

    answer =
      Cronwatch.run("nightly", fn _ -> {:error, :snooze} end, instance: cw, discard_when: &(&1 == :snooze))

    assert answer == {:error, :snooze}
    assert_received {:hung, writer}
    assert runs(cw, "nightly") == []
    ref = Process.monitor(writer)
    release(writer)
    assert_receive {:DOWN, ^ref, :process, _, _}, 1000
    # The hooked store has no delete_run_if, so the row cannot be taken
    # back: it is finished as the run ended, never left running.
    assert [%{status: "failed", error: ":snooze"}] = runs(cw, "nightly")
    assert running(store) == []
    assert messages(errors) == [took_too_long("nightly")]
  end

  defmodule Hanging do
    @moduledoc false
    # A memory store whose first insert_run hangs until released, with
    # delete_run_if, which the hooked store lacks.
    @behaviour Cronwatch.Store

    alias Cronwatch.Store.Memory

    @impl true
    def new(opts, _instance), do: {:ok, Map.new(opts)}
    @impl true
    def init(_h), do: :ok

    @impl true
    def insert_run(h, run) do
      :counters.add(h.inserts, 1, 1)

      if :counters.get(h.inserts, 1) == 1 do
        send(h.test, {:hung, self()})

        receive do
          :release -> :ok
        end
      end

      Memory.insert_run(h.inner, run)
    end

    @impl true
    def upsert_job(h, d, now), do: Memory.upsert_job(h.inner, d, now)
    @impl true
    def get_job(h, n), do: Memory.get_job(h.inner, n)
    @impl true
    def list_jobs(h), do: Memory.list_jobs(h.inner)
    @impl true
    def delete_job(h, n), do: Memory.delete_job(h.inner, n)
    @impl true
    def update_run(h, r), do: Memory.update_run(h.inner, r)
    @impl true
    def update_run_if(h, r, f), do: Memory.update_run_if(h.inner, r, f)
    @impl true
    def delete_run_if(h, i, j, s), do: Memory.delete_run_if(h.inner, i, j, s)
    @impl true
    def get_run(h, i), do: Memory.get_run(h.inner, i)
    @impl true
    def list_runs(h, j, l), do: Memory.list_runs(h.inner, j, l)
    @impl true
    def running_runs(h), do: Memory.running_runs(h.inner)
    @impl true
    def get_state(h, j), do: Memory.get_state(h.inner, j)
    @impl true
    def set_state(h, s), do: Memory.set_state(h.inner, s)
    @impl true
    def compare_and_set_state(h, s, v), do: Memory.compare_and_set_state(h.inner, s, v)
    @impl true
    def prune(h, b), do: Memory.prune(h.inner, b)
  end

  test "a run given back is taken back by its late start write, on a store that can" do
    {Cronwatch.Store.Memory, inner} = Stores.memory()
    %{cw: cw} = make(store: {Hanging, inner: inner, test: self(), inserts: :counters.new(1, [])})

    answer =
      Cronwatch.run("nightly", fn _ -> {:error, :snooze} end, instance: cw, discard_when: &(&1 == :snooze))

    assert answer == {:error, :snooze}
    assert_received {:hung, writer}
    ref = Process.monitor(writer)
    release(writer)
    assert_receive {:DOWN, ^ref, :process, _, _}, 1000
    assert runs(cw, "nightly") == []
  end

  test "giving up on the start write leaves no message, monitor, or link in the caller" do
    %{cw: cw} = hanging()
    {:links, links} = Process.info(self(), :links)

    assert Cronwatch.run("nightly", fn _ -> :done end, instance: cw) == :done
    assert_received {:hung, writer}
    ref = Process.monitor(writer)
    release(writer)
    assert_receive {:DOWN, ^ref, :process, _, _}, 1000

    assert Process.info(self(), :links) == {:links, links}
    assert Process.info(self(), :monitors) == {:monitors, []}
    refute_receive _, 50
  end

  test "a store that fails, as opposed to hangs, is reported at once and the job runs" do
    {hooked, hooks} = Stores.hooked(Stores.memory())
    Stores.hook(hooks, :insert_run, fn _args, _call -> {:error, %RuntimeError{message: "store down"}} end)
    %{cw: cw, errors: errors} = make(store: Stores.option(hooked))
    {micros, answer} = :timer.tc(fn -> Cronwatch.run("nightly", fn _ -> :done end, instance: cw) end)
    assert answer == :done
    assert micros < @deadline * 1000
    assert messages(errors) == ["store down", "store down"]
  end
end
