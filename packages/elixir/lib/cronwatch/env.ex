defmodule Cronwatch.Env do
  @moduledoc false
  # The environment, read in one place and when used, never when a module is
  # compiled: the first of CRONWATCH_ENV, APP_ENV, and MIX_ENV set to more
  # than whitespace, trimmed and lowercased, as every CronWatch library reads
  # it. Mix.env/0 is not read, since Mix is absent from a release.

  @vars ["CRONWATCH_ENV", "APP_ENV", "MIX_ENV"]

  @doc """
  The environment: from the values of the variables that name it, in order,
  the first that holds more than whitespace, trimmed and lowercased, with
  `"prod"` read as `"production"` and `"dev"` and `"local"` as
  `"development"`; nil when none is set. `"test"` and `"testing"` are not
  development: a test run or CI needs `CRON_SECRET` or a token, as
  production does.
  """
  def environment_of(values) do
    Enum.find_value(values, fn
      value when is_binary(value) ->
        case value |> Cronwatch.JS.trim() |> String.downcase() do
          "" -> nil
          "prod" -> "production"
          name when name in ["dev", "local"] -> "development"
          name -> name
        end

      _ ->
        nil
    end)
  end

  @doc "The environment (`environment_of/1`) from CRONWATCH_ENV, APP_ENV, and MIX_ENV."
  def environment, do: @vars |> Enum.map(&System.get_env/1) |> environment_of()

  @doc "Whether this is development."
  def development?, do: environment() == "development"

  @doc "Whether this is production."
  def production?, do: environment() == "production"

  @doc "A variable, with an empty value counting as unset."
  def read(var) do
    case System.get_env(var) do
      "" -> nil
      value -> value
    end
  end

  @doc """
  A secret from the environment (`CRONWATCH_TOKEN`, `CRON_SECRET`), trimmed
  as JavaScript trims: a value read from a file often ends in a newline that
  no header can carry, since HTTP strips the whitespace around a header's
  value. nil when the variable is unset, empty, or only whitespace, so a
  blank value counts as not set and the dashboard and handlers fail closed.
  """
  def read_secret(var), do: var |> System.get_env() |> secret()

  @doc "A secret given as a string, trimmed as `read_secret/1` trims: nil when it is nil, empty, or only whitespace."
  def secret(value) when is_binary(value) do
    case Cronwatch.JS.trim(value) do
      "" -> nil
      trimmed -> trimmed
    end
  end

  def secret(_), do: nil
end
