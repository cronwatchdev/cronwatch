defmodule Cronwatch.Supervisor.Opts do
  @moduledoc false
  # An instance's options as its child spec carries them. A supervisor's
  # reports quote a child's start call, and the options hold credentials
  # (a channel's webhook URL or API key, a store's password, the cron
  # secret) before any of them is read into a struct that hides them, so
  # this one shows only which options were given.

  defstruct opts: []

  defimpl Inspect do
    def inspect(%{opts: opts}, _inspect_opts) do
      keys = if is_list(opts), do: for({k, _} <- opts, is_atom(k), do: k), else: []
      "#Cronwatch.Supervisor.Opts<" <> Enum.map_join(keys, ", ", &Atom.to_string/1) <> ">"
    end
  end
end
