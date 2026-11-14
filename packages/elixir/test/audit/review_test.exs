defmodule Cronwatch.Audit.ReviewTest.Jobs do
  @moduledoc false
  def ran(ctx, _conn, pid) do
    send(pid, {:ran, ctx.job})
    :ok
  end

  def boom(_ctx, _conn), do: raise("boom password=hunter2")
end

defmodule Cronwatch.Audit.ReviewTest do
  @moduledoc """
  The findings of the review after 0.14.3, each held fixed: an instance
  whose processes stop or restart under a run, a job run by a name the
  instance no longer declares, a store call made inside the app's
  transaction, a body a Phoenix endpoint already parsed, and a handler
  asked while the instance is down. The redaction's bound is in
  test/cronwatch/output_assignment_test.exs.
  """
  use ExUnit.Case, async: true

  import Cronwatch.Test.Client
  import Cronwatch.Test.More
  import ExUnit.CaptureLog

  alias Cronwatch.Audit.ReviewTest.Jobs
  alias Cronwatch.JS
  alias Cronwatch.Run.Exec
  alias Cronwatch.Runs
  alias Cronwatch.Store.Ecto, as: EctoStore
  alias Cronwatch.Store.Memory

  # An instance on a memory store that outlives it, as an app's database
  # outlives the instance, and the store's handle to read it by.
  defp outliving(opts \\ []) do
    name = shared_memory()
    {:ok, handle} = Memory.new([server: name], nil)
    Map.put(make([store: {Memory, server: name}] ++ opts), :store, handle)
  end

  defp stored_runs(handle, job) do
    {:ok, runs} = Memory.list_runs(handle, job, 50)
    runs
  end

  defp isolated(cw, job) do
    test = self()

    task =
      Task.async(fn ->
        catch_exit(
          Cronwatch.run(
            job,
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
    task
  end

  # Whether this instance's Cronwatch.Runs wrote a crash report: the log
  # captured is every test's, so another instance's does not count.
  defp crashed?(log, cw), do: log =~ "#{inspect(Runs.server(cw))} terminating"

  defp restart_runs(cw) do
    {old, _} = before = processes(cw)
    Process.exit(old, :kill)
    restarted(cw, before)
  end

  describe "a run's process that dies while the task supervisor is down" do
    test "an isolated run in flight as the instance stops is recorded failed, and nothing crashes" do
      %{cw: cw, store: store} = outliving()
      caller = isolated(cw, "isolated")

      log =
        capture_log(fn ->
          stop_supervised!(cw)
          assert Task.await(caller) == :shutdown
          eventually(fn -> match?([%{status: "failed"}], stored_runs(store, "isolated")) end)
        end)

      assert [%{error: "exit: :shutdown"}] = stored_runs(store, "isolated")
      refute crashed?(log, cw)
    end

    test "Cronwatch.Runs outlives a restart of the processes after it under an isolated run" do
      %{cw: cw, store: store} = outliving()
      caller = isolated(cw, "isolated")
      runs = Process.whereis(Runs.server(cw))
      locks = Process.whereis(Cronwatch.Locks.server(cw))

      log =
        capture_log(fn ->
          Process.exit(locks, :kill)
          assert Task.await(caller) == :shutdown
          eventually(fn -> match?([%{status: "failed"}], stored_runs(store, "isolated")) end)
        end)

      assert Process.whereis(Runs.server(cw)) == runs
      refute crashed?(log, cw)
      # Once the locks are back (a run made before then runs unrecorded).
      eventually(fn -> (pid = Process.whereis(Cronwatch.Locks.server(cw))) && pid != locks end)
      assert Cronwatch.run("after", fn _ -> :done end, instance: cw) == :done
      eventually(fn -> match?([%{status: "ok"}], Cronwatch.runs!("after", 5, instance: cw)) end)
    end
  end

  describe "a job run by a name the instance does not declare" do
    test "keeps the options the store holds after Cronwatch.Runs restarted" do
      %{cw: cw} = make()
      Cronwatch.job!("late", schedule: "0 2 * * *", grace: "15m", timeout: "2h", expect: "done", instance: cw)
      assert {:ok, true} = Cronwatch.sync_job("late", instance: cw)
      before = JS.stringify(Cronwatch.job_summary!("late", instance: cw).definition)

      restart_runs(cw)
      assert Cronwatch.defined_jobs(instance: cw) == []
      assert Cronwatch.run("late", fn _ -> "not it" end, instance: cw) == "not it"

      assert JS.stringify(Cronwatch.job_summary!("late", instance: cw).definition) == before
      # Judged by the stored options too: the expect rule failed the run.
      assert [%{status: "failed", error: "Output did not contain \"done\""}] =
               Cronwatch.runs!("late", 5, instance: cw)
    end

    test "keeps the options another instance stored" do
      %{cw: one, store: store} = outliving()
      %{cw: two} = make(store: {Memory, server: store.server})
      Cronwatch.job!("shared", schedule: "*/5 * * * *", instance: one)
      Cronwatch.run("shared", fn _ -> :ok end, instance: one)
      before = JS.stringify(Cronwatch.job_summary!("shared", instance: one).definition)

      Cronwatch.run("shared", fn _ -> :ok end, instance: two)
      assert JS.stringify(Cronwatch.job_summary!("shared", instance: one).definition) == before
      {:ok, handle} = Cronwatch.start("shared", instance: two)
      Cronwatch.finish(handle)
      assert JS.stringify(Cronwatch.job_summary!("shared", instance: one).definition) == before
    end

    test "a job no one declared is not written over a stored definition that says more" do
      %{cw: one, store: store} = outliving()
      %{cw: two} = make(store: {Memory, server: store.server})
      Cronwatch.job!("shared", schedule: "*/5 * * * *", instance: one)
      {:ok, true} = Cronwatch.sync_job("shared", instance: one)
      before = JS.stringify(Cronwatch.job_summary!("shared", instance: one).definition)

      # Declared bare and marked implicit, as a run by name declares it when
      # the store could not be read for the options.
      {:ok, bare} = Cronwatch.Job.new(two, "shared", [], [])
      Runs.declare(two, %{bare | implicit: true})
      Cronwatch.run("shared", fn _ -> :ok end, instance: two)
      assert JS.stringify(Cronwatch.job_summary!("shared", instance: one).definition) == before
    end

    test "is still declared bare when the store does not know it" do
      %{cw: cw} = make()
      Cronwatch.run("new", fn _ -> :ok end, instance: cw)
      assert JS.stringify(Cronwatch.job_summary!("new", instance: cw).definition) == ~s({"name":"new"})
    end
  end

  test "a run an integration opened is recorded when it closes after Cronwatch.Runs restarted" do
    %{cw: cw} = make()
    job = Cronwatch.job!("worker", timeout: "1h", instance: cw)
    Cronwatch.sync_job("worker", instance: cw)

    for {id, finish} <- [
          {"opened:1", &Exec.close(&1, {:returned, :ok})},
          {"opened:2", &Exec.take_back(&1, {:returned, {:error, :gave_up}})}
        ] do
      state = Exec.open(job, trigger: "oban", id: id, defer: true)
      restart_runs(cw)
      Runs.declare(cw, job)
      finish.(state)
    end

    assert {:ok, %{status: "ok"}} = Cronwatch.get_run("opened:1", instance: cw)
    # Given back, but its row went nowhere: the run is taken back all the same.
    assert {:ok, nil} = Cronwatch.get_run("opened:2", instance: cw)
  end

  test "a store call made apart from the app's transaction leaves no message in a caller that traps exits" do
    Process.flag(:trap_exit, true)
    me = self()
    assert EctoStore.apart(nil, nil, fn -> :stored end) == :stored
    assert [^me | _] = EctoStore.apart(nil, nil, fn -> Process.get(:"$callers") end)
    assert_raise RuntimeError, "no", fn -> EctoStore.apart(nil, nil, fn -> raise "no" end) end
    assert catch_throw(EctoStore.apart(nil, nil, fn -> throw(:it) end)) == :it
    assert catch_exit(EctoStore.apart(nil, nil, fn -> exit(:gone) end)) == :gone
    refute_received _
  after
    Process.flag(:trap_exit, false)
  end

  test "an empty JSON object a Phoenix endpoint's parsers already read is a body with no fields" do
    %{cw: cw} = make()
    Cronwatch.run("x", fn _ -> :ok end, instance: cw)
    opts = Cronwatch.Web.init(instance: cw, token: false)

    post = fn body, params ->
      conn =
        Plug.Test.conn("POST", "/cronwatch/api/jobs/x/silence", body)
        |> Plug.Conn.put_req_header("content-type", "application/json")

      conn = if params, do: %{conn | body_params: params}, else: conn
      Cronwatch.Web.call(conn, opts)
    end

    # Read here: {} is the default hour, as the SDK answers it.
    assert post.("{}", nil).status == 200
    # Read by Plug.Parsers, which leaves nothing to read and an empty map.
    parsed = post.("", %{})
    assert parsed.status == 200
    assert parsed.resp_body =~ ~s("health":"silenced")
    assert post.("", %{"for" => "2h"}).status == 200
    assert post.("", %{"_json" => [1]}).status == 400
    # Never read by anyone: still no JSON at all.
    assert post.("", nil).status == 400
  end

  describe "a handler asked while its instance is not running" do
    # Stops the instance. Its one "ran unrecorded" warning is marked as
    # given first: other tests, run alongside, count that line in the log
    # they capture, which is every test's.
    defp stop_quietly(cw) do
      :persistent_term.put({Cronwatch, :unavailable_reported, cw}, true)
      stop_supervised!(cw)
    end

    defp ask(handler, headers \\ []) do
      headers
      |> Enum.reduce(Plug.Test.conn("GET", "/cron/nightly"), fn {k, v}, conn ->
        Plug.Conn.put_req_header(conn, k, v)
      end)
      |> Cronwatch.Handler.call(handler)
    end

    test "still asks for the secret, and runs the job unrecorded" do
      %{cw: cw} = make(cron_secret: "from-the-instance")
      secret = "s3" <> "cret"
      handler = Cronwatch.Handler.init(instance: cw, job: "nightly", run: {Jobs, :ran, [self()]}, secret: secret)
      stop_quietly(cw)

      assert ask(handler).status == 401
      assert ask(handler, [{"authorization", "Bearer wrong"}]).status == 401
      refute_received {:ran, _}

      capture_log(fn ->
        answer = ask(handler, [{"authorization", "Bearer " <> secret}])
        assert answer.status == 200
        assert answer.halted
        assert_received {:ran, "nightly"}

        assert %{"ok" => true, "job" => "nightly", "run" => nil, "status" => "ok"} =
                 answer.resp_body |> JS.parse!() |> JS.Object.to_list() |> Map.new()

        failing = Cronwatch.Handler.init(instance: cw, job: "nightly", run: {Jobs, :boom, []}, secret: secret)
        failed = ask(failing, [{"authorization", "Bearer " <> secret}])
        assert failed.status == 500
        body = failed.resp_body |> JS.parse!() |> JS.Object.to_list() |> Map.new()
        assert body["status"] == "failed"
        assert body["error"] == "RuntimeError: boom password=[redacted]"
      end)
    end

    test "does not take the instance's own secret on trust: it is refused without one of its own" do
      %{cw: cw} = make(cron_secret: "from-the-instance")
      handler = Cronwatch.Handler.init(instance: cw, job: "nightly", run: {Jobs, :ran, [self()]})
      stop_quietly(cw)

      if System.get_env("CRON_SECRET") in [nil, ""] do
        assert ask(handler, [{"authorization", "Bearer from-the-instance"}]).status == 503
        refute_received {:ran, _}
      end
    end
  end

  describe "the SDK's client fixes of the same review" do
    alias Cronwatch.Test.Flaky
    alias Cronwatch.Test.Stores

    test "an error handler that raises does not stop a check or a read" do
      {store, broken} = Flaky.new(Stores.memory())
      %{cw: cw} = make(store: store, on_error: fn _, _ -> raise "handler down" end)
      Cronwatch.job!("a", schedule: "* * * * *", instance: cw)
      Cronwatch.job!("b", instance: cw)
      Cronwatch.run("a", fn _ -> :ok end, instance: cw)
      {:ok, handle} = Cronwatch.start("b", instance: cw)

      # Every place a check and a read report from: each job's runs, the
      # pending runs, the pruning, and a flush and a finish.
      Flaky.break(broken, [:list_runs, :prune, :get_run, :get_state])
      assert {:ok, result} = Cronwatch.check(instance: cw)
      assert Enum.map(result.jobs, & &1.name) == ["a", "b"]
      assert {:ok, [_, _]} = Cronwatch.jobs_with_runs(5, instance: cw)
      assert Cronwatch.flush(handle) == :ok
      assert Cronwatch.finish(handle) == nil

      Flaky.mend(broken)
      assert {:ok, %{jobs: [%{health: _}, _]}} = Cronwatch.check(instance: cw)
      assert %{status: "ok"} = Cronwatch.finish(handle)
    end

    test "a secret that starts in one flush and ends in the next is redacted whole" do
      %{cw: cw} = make()
      {:ok, run} = Cronwatch.start("deploy", instance: cw)
      output = fn -> Cronwatch.get_run!(run.id, instance: cw).output end

      Cronwatch.log(run, "starting")
      Cronwatch.log(run, "-----BEGIN PRIVATE KEY-----")
      Cronwatch.log(run, "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7VJTUt9Us8cKj")
      Cronwatch.flush(run)
      assert output.() == "starting\n[redacted]"

      Cronwatch.log(run, "MFNRV0X0e1nSecondHalfOfTheKeySecondHalfOfTheKeySecondHalfOfTheKe")
      Cronwatch.log(run, "-----END PRIVATE KEY-----")
      Cronwatch.log(run, "api_token:")
      Cronwatch.flush(run)
      assert output.() == "starting\n[redacted]\napi_token:"

      # A label flushed, then its value: at the next flush, and again at finish.
      Cronwatch.log(run, "hunter2hunter2")
      Cronwatch.flush(run)
      assert output.() == "starting\n[redacted]\napi_token:\n[redacted]"
      Cronwatch.log(run, "password:")
      Cronwatch.flush(run)
      Cronwatch.log(run, "correct-horse")
      finished = Cronwatch.finish(run)
      assert finished.output == "starting\n[redacted]\napi_token:\n[redacted]\npassword:\n[redacted]"
      refute inspect(Cronwatch.get_run!(run.id, instance: cw), limit: :infinity) =~ ~r/SecondHalf|hunter2|correct-horse/
    end

    test "flushes after another process wrote to the run still append, each redacted" do
      %{cw: cw} = make()
      job = Cronwatch.job!("deploy", instance: cw)
      {:ok, run} = Cronwatch.start(job)
      Cronwatch.log(run, "one")
      Cronwatch.flush(run)

      # Another handle on the same run (another process) appends in between.
      {:ok, other} = Cronwatch.resume(job, run.id)
      Cronwatch.log(other, "from elsewhere")
      Cronwatch.flush(other)
      Cronwatch.log(run, "token=abc123abc123")
      Cronwatch.flush(run)
      assert Cronwatch.get_run!(run.id, instance: cw).output == "one\nfrom elsewhere\ntoken=[redacted]"

      # A long run: the remembered tail stays bounded and the output capped.
      for i <- 0..5 do
        Cronwatch.log(run, "line #{i} " <> String.duplicate("x", 6000))
        Cronwatch.flush(run)
      end

      Cronwatch.log(run, "secret=tail-secret-value")
      finished = Cronwatch.finish(run)
      assert JS.len16(finished.output) <= 16 * 1024 + 40
      assert finished.output =~ ~r/secret=\[redacted\]\z/
      refute finished.output =~ "tail-secret-value"
    end

    test "a handle kept across a forget declares the job again on its next run, and writes it once" do
      {store, hooks} = Stores.hooked(Stores.memory())
      upserts = :counters.new(1, [])

      Stores.hook(hooks, :upsert_job, fn _args, call ->
        :counters.add(upserts, 1, 1)
        call.()
      end)

      %{cw: cw} = make(store: Stores.option(store))
      job = Cronwatch.job!("nightly", schedule: "0 2 * * *", description: "Nightly export", instance: cw)
      Cronwatch.run(job, fn _ -> :ok end)
      assert :counters.get(upserts, 1) == 1
      :ok = Cronwatch.forget("nightly", instance: cw)
      assert Cronwatch.defined_jobs(instance: cw) == []

      Cronwatch.run(job, fn _ -> :ok end)
      assert :counters.get(upserts, 1) == 2
      assert Enum.map(Cronwatch.defined_jobs(instance: cw), & &1.name) == ["nightly"]
      Cronwatch.run(job, fn _ -> :ok end)
      Cronwatch.run(job, fn _ -> :ok end)
      assert :counters.get(upserts, 1) == 2, "written once, not on every run"

      assert JS.Object.get(Cronwatch.job_summary!("nightly", instance: cw).definition, "description") ==
               "Nightly export"
    end

    test "a forget that lands while a check or a read is writing declarations is not undone by it" do
      for read <- [:check, :jobs, :job_summary] do
        %{cw: cw, store: store} = outliving()
        c = Cronwatch.Config.get(cw)
        Cronwatch.job!("keep", schedule: "0 2 * * *", instance: cw)
        Cronwatch.job!("nightly", schedule: "0 2 * * *", instance: cw)
        locks = Cronwatch.Locks.server(cw)

        # Neither is written yet, so the read's own loop is what would write
        # them. It has picked "nightly" up and waits its turn to write it
        # when the forget lands.
        reading =
          Cronwatch.Core.declaring(c, "nightly", fn ->
            task =
              Task.async(fn ->
                case read do
                  :check -> Cronwatch.check(instance: cw)
                  :jobs -> Cronwatch.jobs(instance: cw)
                  :job_summary -> Cronwatch.job_summary("nightly", instance: cw)
                end
              end)

            eventually(fn ->
              match?(%{{:sync, "nightly"} => {_, queue}} when queue != {[], []}, :sys.get_state(locks).locks)
            end)

            :ok = Cronwatch.forget("nightly", instance: cw)
            task
          end)

        assert {:ok, _} = Task.await(reading)
        assert Enum.map(Cronwatch.defined_jobs(instance: cw), & &1.name) == ["keep"], "#{read}: still forgotten here"
        assert Memory.get_job(store, "nightly") == {:ok, nil}, "#{read}: not written again"
        assert Memory.get_state(store, "nightly") == {:ok, nil}, "#{read}: no state left behind"

        # Its next run still brings it back.
        Cronwatch.run("nightly", fn _ -> :ok end, instance: cw)
        assert {:ok, %{name: "nightly"}} = Memory.get_job(store, "nightly")
      end
    end

    test "a handle kept across a restart of Cronwatch.Runs declares the job again as the handle holds it" do
      %{cw: cw} = make()
      job = Cronwatch.job!("nightly", schedule: "0 2 * * *", instance: cw)
      Cronwatch.run(job, fn _ -> :ok end)
      before = JS.stringify(Cronwatch.job_summary!("nightly", instance: cw).definition)
      restart_runs(cw)

      Cronwatch.run(job, fn _ -> :ok end)
      assert Enum.map(Cronwatch.defined_jobs(instance: cw), & &1.name) == ["nightly"]
      assert Runs.synced?(cw, "nightly")
      assert JS.stringify(Cronwatch.job_summary!("nightly", instance: cw).definition) == before
    end
  end

  describe "stuck stays open until a run finishes ok" do
    alias Cronwatch.Evaluate
    alias Cronwatch.Test.Capture
    alias Cronwatch.Test.Clock

    @min 60_000

    test "a job that hangs on every run alerts stuck once, and recovers on its first success" do
      %{cw: cw, clock: clock, alerts: alerts} = make()
      job = Cronwatch.job!("hangs", schedule: "*/5 * * * *", timezone: "UTC", timeout: "10m", instance: cw)

      # Twelve runs, five minutes apart, none of which ever finishes.
      handles =
        for _ <- 1..12 do
          {:ok, handle} = Cronwatch.start(job)
          Clock.advance(clock, 5 * @min)
          Cronwatch.check!(instance: cw)
          handle
        end

      assert Capture.types(alerts) == ["stuck"]
      assert Cronwatch.job_summary!("hangs", instance: cw).health == "stuck"

      # A run that fails alerts as failed by its own rule; stuck stays open beside it.
      assert_raise RuntimeError, fn -> Cronwatch.run(job, fn _ -> raise "boom" end) end
      assert Capture.types(alerts) == ["stuck", "failed"]
      assert Enum.sort(Cronwatch.job_summary!("hangs", instance: cw).open) == ["failed", "stuck"]

      # A hung run that reports failing late is not counted again and alerts nothing.
      Cronwatch.fail(hd(handles), "late")
      assert Capture.types(alerts) == ["stuck", "failed"]

      # The first success closes both with one recovery.
      Cronwatch.run(job, fn _ -> :ok end)
      assert Capture.types(alerts) == ["stuck", "failed", "recovered"]
      assert Enum.sort(List.last(Capture.alerts(alerts)).details.after) == ["failed", "stuck"]
      assert Cronwatch.job_summary!("hangs", instance: cw).open == []
    end

    test "a stuck run that finishes ok late closes stuck with a recovery, though other runs started meanwhile" do
      %{cw: cw, clock: clock, alerts: alerts} = make()
      job = Cronwatch.job!("slow-queue", timeout: "10m", instance: cw)
      {:ok, first} = Cronwatch.start(job)
      Clock.advance(clock, 11 * @min)
      Cronwatch.check!(instance: cw)
      assert Capture.types(alerts) == ["stuck"]
      {:ok, second} = Cronwatch.start(job)
      assert Cronwatch.job_summary!("slow-queue", instance: cw).open == ["stuck"], "a start leaves stuck open"
      Cronwatch.finish(first)
      assert Capture.types(alerts) == ["stuck", "recovered"]
      Cronwatch.finish(second)
      assert Capture.types(alerts) == ["stuck", "recovered"]
    end

    test "a run starting closes missed and leaves stuck open since it opened" do
      %{cw: cw} = make()
      job = Cronwatch.job!("j", timeout: "10m", instance: cw)
      t0 = Clock.t0()
      run = %Cronwatch.Run{id: "r", job: "j", status: "running", started_at: t0 + @min}
      state = %{Cronwatch.JobState.new("j") | open: [{"stuck", t0}, {"missed", t0 + 1}]}
      assert Evaluate.on_run_start(job.definition, run, state, t0 + @min).open == [{"stuck", t0}]
    end
  end
end
