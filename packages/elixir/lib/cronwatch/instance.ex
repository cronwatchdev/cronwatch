defmodule Cronwatch.Instance do
  @moduledoc false
  # Holds an instance's config (Cronwatch.Config.put/1) for as long as the
  # instance runs, and erases it when the instance stops. It is the
  # supervisor's first child: it starts before any child reads the config,
  # and under :rest_for_one it is stopped after them and is not restarted
  # when one of them is, so a crash of the store's process or of
  # Cronwatch.Runs leaves the config in place for the children restarted
  # after it.

  use GenServer

  alias Cronwatch.Config

  @doc false
  def child_spec(config), do: %{id: __MODULE__, start: {__MODULE__, :start_link, [config]}}

  @doc false
  def start_link(config), do: GenServer.start_link(__MODULE__, config)

  @impl true
  def init(config) do
    # So the instance's shutdown reaches terminate/2.
    Process.flag(:trap_exit, true)
    Config.put(config)
    {:ok, config.name}
  end

  @impl true
  def terminate(_reason, name) do
    Config.erase(name)
    :ok
  end
end
