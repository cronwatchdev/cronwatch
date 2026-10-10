defmodule Cronwatch.RestartTest do
  @moduledoc """
  An instance whose processes crash: the config outlives a restart of the
  store's process or of Cronwatch.Runs, the instance keeps recording, and a
  run made while it is down still runs.
  """
  use ExUnit.Case, async: true

  @moduletag :capture_log

  import Cronwatch.Test.Client
  import ExUnit.CaptureLog

  alias Cronwatch.Runs

  defp runs(cw, job), do: Cronwatch.runs!(job, 50, instance: cw)

  defp supervisor(cw), do: Process.whereis(Module.concat(cw, Supervisor))

  # Crashes Cronwatch.Runs through a call it has no clause for, so its
  # terminate/2 runs, as it does for any crash but a kill.
  defp crash_runs(cw) do
    old = Process.whereis(Runs.server(cw))
    catch_exit(GenServer.call(old, :no_such_call))
    eventually(fn -> (pid = Process.whereis(Runs.server(cw))) && pid != old end)
  end

  test "a crash of the store's process restarts the rest, and the instance keeps recording" do
    %{cw: cw} = make()
    sup = supervisor(cw)
    assert Cronwatch.run("before", fn _ -> :ok end, instance: cw) == :ok

    {_, store, _, _} = Enum.find(Supervisor.which_children(sup), &(elem(&1, 0) == Cronwatch.Store.Memory))
    old_runs = Process.whereis(Runs.server(cw))
    Process.exit(store, :kill)
    eventually(fn -> (pid = Process.whereis(Runs.server(cw))) && pid != old_runs end)

    assert supervisor(cw) == sup
    assert Cronwatch.run("after", fn _ -> :done end, instance: cw) == :done
    eventually(fn -> match?([%{status: "ok"}], runs(cw, "after")) end)
    assert {:ok, _} = Cronwatch.check(instance: cw)
  end

  test "a crash of Cronwatch.Runs restarts it, and the instance keeps recording" do
    %{cw: cw} = make()
    sup = supervisor(cw)
    crash_runs(cw)

    assert supervisor(cw) == sup
    assert Cronwatch.run("after", fn _ -> :done end, instance: cw) == :done
    eventually(fn -> match?([%{status: "ok"}], runs(cw, "after")) end)
  end

  test "an instance that stops takes its config with it" do
    %{cw: cw} = make()
    stop_supervised!(cw)
    assert_raise ArgumentError, ~r/no Cronwatch instance named/, fn -> Cronwatch.Config.get(cw) end
  end

  describe "an instance that is not running" do
    test "runs the job unrecorded, hands back what it did, and says so once" do
      instance = :"cw_down#{System.unique_integer([:positive])}"

      log =
        capture_log(fn ->
          answer =
            Cronwatch.run(
              "nightly",
              fn ctx ->
                Cronwatch.log(ctx, "still logs nowhere")
                Cronwatch.metric(ctx, "rows", 1)
                assert Cronwatch.current() == ctx
                :done
              end,
              instance: instance
            )

          assert answer == :done
          assert Cronwatch.run("nightly", fn _ -> :again end, instance: instance) == :again
        end)

      assert log =~ "#{inspect(instance)} is not running, so nightly ran unrecorded"
      assert length(String.split(log, "ran unrecorded")) == 2
      assert Cronwatch.current() == nil

      assert_raise RuntimeError, "boom", fn ->
        Cronwatch.run("nightly", fn _ -> raise "boom" end, instance: instance)
      end

      assert catch_exit(Cronwatch.run("nightly", fn _ -> exit(:gone) end, instance: instance, isolate: true)) == :gone
      assert Cronwatch.run("nightly", fn _ -> {:error, :no} end, instance: instance, isolate: true) == {:error, :no}
    end

    test "after it has stopped" do
      %{cw: cw} = make()
      {:ok, job} = Cronwatch.job("nightly", instance: cw)
      stop_supervised!(cw)

      capture_log(fn ->
        assert Cronwatch.run(job, fn _ -> :ran end) == :ran
      end)
    end
  end

  test "an isolated run whose Cronwatch.Runs restarted meanwhile is recorded, not left running" do
    %{cw: cw} = make()
    test = self()

    caller =
      Task.async(fn ->
        catch_exit(
          Cronwatch.run(
            "isolated",
            fn _ ->
              send(test, :running)
              Process.sleep(:infinity)
            end,
            instance: cw,
            isolate: true
          )
        )
      end)

    assert_receive :running
    crash_runs(cw)

    # The restart takes the instance's tasks down, the function's with them.
    assert Task.await(caller) == :shutdown
    eventually(fn -> match?([%{status: "failed"}], runs(cw, "isolated")) end)
    assert [%{error: "exit: :shutdown"}] = runs(cw, "isolated")
  end
end
