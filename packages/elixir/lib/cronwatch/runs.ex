defmodule Cronwatch.Runs do
  @moduledoc false
  # The instance's tables and the monitor on every run in flight.
  #
  # It owns the ETS tables: the declared jobs (name to Job, written through
  # this process so two declarations of one name do not race), the runs in
  # flight (id to what recording one needs), the processes running them (for
  # current/0 through $callers), the lines and metrics runs and handles
  # collect (Cronwatch.Lines), the handles' own state, and a few flags. It
  # monitors the process of every run it holds, and records a run whose
  # process died at once, as failed, with the exit reason. It flips a run's
  # cancelled flag at the job's timeout.

  use GenServer

  alias Cronwatch.Config
  alias Cronwatch.Lines
  alias Cronwatch.Run.Exec

  # The longest one timer waits (about 49.7 days); a longer timeout is
  # waited out in steps. The tests shorten it through the application
  # environment (:max_timer), which apps leave alone.
  @max_timer 4_294_967_295

  defp max_timer, do: Application.get_env(:cronwatch, :max_timer, @max_timer)

  @doc false
  def child_spec(name), do: %{id: __MODULE__, start: {__MODULE__, :start_link, [name]}}

  @doc false
  def start_link(name), do: GenServer.start_link(__MODULE__, name, name: server(name))

  @doc false
  def server(instance), do: Module.concat(instance, Runs)

  @doc "An instance's table."
  def table(instance, which), do: :"#{instance}.cronwatch.#{which}"

  ## Jobs

  @doc "Declares a job: stores it, not yet written to the store."
  def declare(instance, job), do: GenServer.call(server(instance), {:declare, job}, :infinity)

  @doc """
  Forgets a declared job (`Cronwatch.forget/2`), keeping its declaration so
  that `declared/2` can declare it again as it was written.
  """
  def undeclare(instance, name), do: GenServer.call(server(instance), {:undeclare, name}, :infinity)

  @doc """
  The declared job, else one forgotten here (`undeclare/2`), declared again
  as it was, else nil: so `Cronwatch.run/3` and `Cronwatch.resume_run/3` by
  name after a forget declare the job as it was written, not bare.
  """
  def declared(instance, name) do
    case job(instance, name) do
      nil ->
        if :ets.member(table(instance, :forgotten), name),
          do: GenServer.call(server(instance), {:redeclare, name}, :infinity),
          else: nil

      job ->
        job
    end
  end

  @doc "The declared job, or nil."
  def job(instance, name) do
    case :ets.lookup(table(instance, :jobs), name) do
      [{_, job, _}] -> job
      [] -> nil
    end
  end

  @doc "Every declared job, by name."
  def jobs(instance) do
    instance |> table(:jobs) |> :ets.tab2list() |> Enum.map(&elem(&1, 1)) |> Enum.sort_by(& &1.name)
  end

  @doc "Whether the declared job has been written to the store since it was declared."
  def synced?(instance, name) do
    match?([{_, _, true}], :ets.lookup(table(instance, :jobs), name))
  end

  @doc """
  Marks the declared job written, when it is still the same declaration:
  through this process, as a declaration is, so one made meanwhile is never
  the one marked.
  """
  def mark_synced(instance, job), do: GenServer.call(server(instance), {:synced, job}, :infinity)

  @doc "Marks the declared job as still to be written: another process forgot it."
  def unmark_synced(instance, name), do: GenServer.call(server(instance), {:unsynced, name}, :infinity)

  ## Flags

  @doc "Sets a flag, answering whether it was newly set."
  def flag(instance, name), do: :ets.insert_new(table(instance, :flags), {name})

  @doc "Whether a flag is set."
  def flag?(instance, name), do: :ets.member(table(instance, :flags), name)

  @doc "Clears a flag."
  def unflag(instance, name), do: :ets.delete(table(instance, :flags), name)

  ## Runs

  @doc """
  Registers a run in flight: its process is monitored, and its cancelled
  flag is set at `timeout_ms`.
  """
  def register(instance, run_id, pid, info, timeout_ms) do
    call(instance, {:register, run_id, pid, info, timeout_ms}, :unavailable)
  end

  @doc """
  Closes a run, answering whether it was still open. While this process is
  down (restarting after a crash), the run it held is gone with its tables,
  so the answer is false.
  """
  def close(instance, run_id), do: call(instance, {:close, run_id}, false)

  # A call on the run path, which must not take a job down while this
  # process is restarting: `default` when it is not there to answer.
  defp call(instance, message, default) do
    GenServer.call(server(instance), message, :infinity)
  catch
    :exit, {reason, _} when reason in [:noproc, :normal, :shutdown, :killed] -> default
    :exit, {{:shutdown, _}, _} -> default
  end

  @doc "The info of a run in flight, or nil."
  def info(instance, run_id) do
    case :ets.lookup(table(instance, :runs), run_id) do
      [{_, _pid, _ref, info}] -> info
      [] -> nil
    end
  end

  @doc "The ids of the runs a process is running, innermost last."
  def runs_of(instance, pid) do
    for {_, id} <- :ets.lookup(table(instance, :pids), pid), do: id
  end

  ## Handles

  @doc "Registers a handle's state, owned by `owner`, dropped when it ends."
  def register_handle(instance, id, owner, state) do
    GenServer.call(server(instance), {:handle, id, owner, state}, :infinity)
  end

  @doc "A handle's state, or nil once its owner ended."
  def handle(instance, id) do
    case :ets.lookup(table(instance, :handles), id) do
      [{_, _owner, state}] -> state
      [] -> nil
    end
  end

  @doc """
  Forgets a handle that is done with (finished, or found finished), and the
  lines it held, rather than keep it until its owner ends: a long-lived
  process that starts and finishes a run a minute would otherwise fill the
  table.
  """
  def drop_handle(instance, id), do: GenServer.call(server(instance), {:drop_handle, id}, :infinity)

  @doc "Changes a handle's state."
  def put_handle(instance, id, state) do
    :ets.update_element(table(instance, :handles), id, {3, state})
  end

  ## The process

  @impl true
  def init(instance) do
    Process.flag(:trap_exit, true)
    opts = [:named_table, :public, read_concurrency: true]
    :ets.new(table(instance, :jobs), [:set | opts])
    :ets.new(table(instance, :runs), [:set | opts])
    :ets.new(table(instance, :pids), [:bag | opts])
    :ets.new(table(instance, :lines), [:ordered_set, :named_table, :public, write_concurrency: true])
    :ets.new(table(instance, :handles), [:set | opts])
    :ets.new(table(instance, :flags), [:set | opts])
    :ets.new(table(instance, :forgotten), [:set | opts])
    config = Config.get(instance)
    for job <- config.jobs, do: :ets.insert(table(instance, :jobs), {job.name, job, false})
    {:ok, %{instance: instance, runs: %{}, owners: %{}}}
  end

  @impl true
  def handle_call({:declare, job}, _from, s) do
    :ets.insert(table(s.instance, :jobs), {job.name, job, false})
    :ets.delete(table(s.instance, :forgotten), job.name)
    {:reply, :ok, s}
  end

  def handle_call({:undeclare, name}, _from, s) do
    case :ets.lookup(table(s.instance, :jobs), name) do
      [{_, job, _}] -> :ets.insert(table(s.instance, :forgotten), {name, job})
      [] -> :ok
    end

    :ets.delete(table(s.instance, :jobs), name)
    {:reply, :ok, s}
  end

  # A forgotten job declared again as it was, unless it was declared anew
  # meanwhile: then that declaration.
  def handle_call({:redeclare, name}, _from, s) do
    job =
      case {:ets.lookup(table(s.instance, :jobs), name), :ets.lookup(table(s.instance, :forgotten), name)} do
        {[{_, job, _}], _} ->
          job

        {[], [{_, job}]} ->
          :ets.insert(table(s.instance, :jobs), {name, job, false})
          job

        {[], []} ->
          nil
      end

    :ets.delete(table(s.instance, :forgotten), name)
    {:reply, job, s}
  end

  def handle_call({:synced, job}, _from, s) do
    t = table(s.instance, :jobs)

    marked =
      case :ets.lookup(t, job.name) do
        [{_, ^job, _}] -> :ets.update_element(t, job.name, {3, true})
        _ -> false
      end

    {:reply, marked, s}
  end

  def handle_call({:unsynced, name}, _from, s) do
    t = table(s.instance, :jobs)
    if :ets.member(t, name), do: :ets.update_element(t, name, {3, false})
    {:reply, :ok, s}
  end

  def handle_call({:register, id, pid, info, timeout_ms}, _from, s) do
    ref = Process.monitor(pid)
    :ets.insert(table(s.instance, :runs), {id, pid, ref, info})
    :ets.insert(table(s.instance, :pids), {pid, id})
    timer = arm(ref, timeout_ms)
    {:reply, :ok, %{s | runs: Map.put(s.runs, ref, {id, pid, timer})}}
  end

  def handle_call({:close, id}, _from, s) do
    case :ets.lookup(table(s.instance, :runs), id) do
      [{_, pid, ref, _}] ->
        Process.demonitor(ref, [:flush])
        {_, s} = drop_run(s, ref, id, pid)
        {:reply, true, s}

      [] ->
        {:reply, false, s}
    end
  end

  def handle_call({:handle, id, owner, state}, _from, s) do
    :ets.insert(table(s.instance, :handles), {id, owner, state})

    owners =
      case s.owners do
        %{^owner => {ref, ids}} -> Map.put(s.owners, owner, {ref, [id | ids]})
        _ -> Map.put(s.owners, owner, {Process.monitor(owner), [id]})
      end

    {:reply, :ok, %{s | owners: owners}}
  end

  def handle_call({:drop_handle, id}, _from, s) do
    owners =
      case :ets.lookup(table(s.instance, :handles), id) do
        [{_, owner, _}] ->
          :ets.delete(table(s.instance, :handles), id)
          Lines.close(table(s.instance, :lines), {:handle, id})

          case s.owners do
            %{^owner => {ref, [^id]}} ->
              Process.demonitor(ref, [:flush])
              Map.delete(s.owners, owner)

            %{^owner => {ref, ids}} ->
              Map.put(s.owners, owner, {ref, List.delete(ids, id)})

            _ ->
              s.owners
          end

        [] ->
          s.owners
      end

    {:reply, :ok, %{s | owners: owners}}
  end

  @impl true
  def handle_info({:DOWN, ref, :process, pid, reason}, s) do
    case s.runs do
      %{^ref => {id, _pid, _timer}} ->
        info = info(s.instance, id)
        {_, s} = drop_run(s, ref, id, pid)
        record_dead(s.instance, id, info, reason)
        {:noreply, s}

      _ ->
        case s.owners do
          %{^pid => {^ref, ids}} ->
            for id <- ids do
              :ets.delete(table(s.instance, :handles), id)
              Lines.close(table(s.instance, :lines), {:handle, id})
            end

            {:noreply, %{s | owners: Map.delete(s.owners, pid)}}

          _ ->
            {:noreply, s}
        end
    end
  end

  # A timeout is tagged with the run's monitor, so one left over from a run
  # already gone (whose id a later run may have taken) does nothing, and a
  # timeout too long for one timer is re-armed with its new timer kept, so
  # dropping the run cancels it.
  def handle_info({:timeout, ref, left}, s) do
    case s.runs do
      %{^ref => {id, pid, _timer}} when left > 0 ->
        {:noreply, %{s | runs: Map.put(s.runs, ref, {id, pid, arm(ref, left)})}}

      %{^ref => {id, _pid, _timer}} ->
        case info(s.instance, id) do
          %{cancel: cancel} -> :atomics.put(cancel, 1, 1)
          _ -> :ok
        end

        {:noreply, s}

      _ ->
        {:noreply, s}
    end
  end

  def handle_info(_msg, s), do: {:noreply, s}

  # A timeout no double holds (refused when a job is declared, so only a
  # store's foreign definition could hold one) never fires, rather than
  # crash this process and the instance with it.
  defp arm(ref, ms) do
    if Cronwatch.Duration.double?(ms), do: arm_ms(ref, ms)
  end

  defp arm_ms(ref, ms) do
    ms = Cronwatch.JS.to_int(Float.ceil(ms * 1.0))
    wait = min(ms, max_timer())
    Process.send_after(self(), {:timeout, ref, ms - wait}, wait)
  end

  defp drop_run(s, ref, id, pid) do
    case Map.pop(s.runs, ref) do
      {{_, _, timer}, runs} ->
        if timer, do: Process.cancel_timer(timer)
        :ets.delete(table(s.instance, :runs), id)
        :ets.delete_object(table(s.instance, :pids), {pid, id})
        {true, %{s | runs: runs}}

      {nil, _} ->
        {false, s}
    end
  end

  # A process that dies inside a job's function has its run recorded as
  # failed at once, in a task of the instance, rather than left running to
  # be reported stuck at its timeout.
  defp record_dead(_instance, _id, nil, _reason), do: :ok

  defp record_dead(instance, id, info, reason) do
    Task.Supervisor.start_child(Cronwatch.Supervisor.tasks(instance), fn ->
      Exec.record_dead(instance, id, info, reason)
    end)
  end
end
