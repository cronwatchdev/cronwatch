defmodule Cronwatch.OutputAssignmentTest do
  # The key=value redaction is matched by hand (Cronwatch.Output.Assignment),
  # since the engine takes seconds on text made to make it backtrack. These
  # hold it to the SDK's pattern as Cronwatch.JSRE runs it, unit for unit,
  # and to a bound on the text that was slow.
  use ExUnit.Case, async: true

  alias Cronwatch.JS
  alias Cronwatch.Output
  alias Cronwatch.Output.Assignment

  @redacted JS.units("[redacted]")

  defp by_hand(text), do: text |> JS.units() |> Assignment.replace(@redacted) |> JS.from_units()
  defp by_pattern(text), do: text |> JS.units() |> Output.assignment_by_pattern() |> JS.from_units()

  @pieces [
            ~w(secret token passwd password pwd apikey api_key api-key accesskey access_key access-key),
            ~w(privatekey private_key private-key credential SECRET Token PassWord PWD Api_Key CREDENTIAL),
            ~w(tokens TOKENS Tokens max_tokens passw passwor api_ access- private key pw secre),
            ~w(a b Z 0 9 _ - -- x_ -y name my- DB_ long_name_of_forty_characters_exactly_here_),
            ["=", ":", "=>", "= ", " : ", "  =>  ", "=   ", "    =", "=    ", "=\t\n", " = ", "﻿:"],
            ["\"", "'", "\"\"", "''", "\"v\"", "'v'", "\"a b\"", "'a b'", "\"a\nb\"", "\"open", "'open"],
            [",", ";", "&", " ", "\n", ".", "/", "@", ">", "<", "(", ")", "{", "}", "[", "]", "é", "ſ", "K", "😀"],
            ["value", "s3cr3t", "v,w", "v;w", "v&w", "v w", "hunter2", "123"]
          ]
          |> List.flatten()

  defp generated(seed, count) do
    :rand.seed(:exsss, {seed, seed * 7 + 1, seed * 13 + 2})
    for _ <- 1..count, do: Enum.map_join(1..:rand.uniform(14), fn _ -> Enum.random(@pieces) end)
  end

  test "it answers what the SDK's pattern answers, over generated text" do
    for seed <- 1..4, text <- generated(seed, 5_000) do
      assert by_hand(text) == by_pattern(text), "differs on #{inspect(text)}"
    end
  end

  test "it answers what the pattern answers at the pattern's own limits" do
    name = String.duplicate("n", 40)
    long = String.duplicate("v", 4096)

    texts =
      for key <- ["pwd", "password", "private_key", "tokens", "xtokens", "token"],
          before <- ["", name, "n" <> name, "-" <> name, name <> "-", "a " <> name, "a-" <> name, "é" <> name],
          rest <- ["", name, name <> "n", String.slice(name, 1..-1//1) <> "-", "tokens", name <> "tokens"],
          value <- [
            "=v",
            "=>v",
            "=>,",
            "=>",
            "=",
            ": 'v'",
            "\"=\"v\"",
            "\" : v",
            "=\"" <> long <> "\"",
            "=\"" <> long <> "v\"",
            "='" <> long <> "v'",
            "=" <> long <> "vv",
            "=\"" <> long,
            "=\" v",
            "=' v",
            "=\"'v'\"",
            "=😀😀,"
          ],
          do: before <> key <> rest <> value <> " tail token=again"

    for text <- texts do
      assert by_hand(text) == by_pattern(text), "differs on #{inspect(String.slice(text, 0, 200))}"
    end
  end

  test "a match starts where the pattern starts it after one before it" do
    for text <- [
          "pwd=apwd=b pwd=c",
          "pwd=a-pwd=b",
          "token=x_secret=y;secret=z",
          "a.pwd=1&b.pwd=2&c-pwd=3",
          "pwd=\"a\"pwd=\"b\"",
          "pwd=" <> String.duplicate("v", 4096) <> "pwd=next",
          "pwd=" <> String.duplicate("v", 4096) <> "-pwd=next"
        ] do
      assert by_hand(text) == by_pattern(text), "differs on #{inspect(String.slice(text, 0, 80))}"
    end
  end

  # Each of these took the engine from one to seven seconds for 68 KB. The
  # bound is loose, for a slow machine; by hand they take milliseconds.
  test "text made to make the pattern backtrack is redacted in bounded time" do
    cases = [
      String.duplicate("pwd-", 17_000),
      String.duplicate("apikey-", 9_700),
      String.duplicate("a-", 34_000),
      # A separator every run, with no value after it, so no match is skipped.
      String.duplicate(String.duplicate("pwd-", 20) <> ":,", 830),
      String.duplicate(String.duplicate("pwd-", 20) <> "=>;", 820),
      String.duplicate("pwd=\" ", 11_000),
      String.duplicate("pwd=' ", 11_000) <> "\n"
    ]

    for text <- cases do
      {micros, out} = :timer.tc(fn -> Output.redact_and_cap(text, &Output.redact_secrets/1) end)
      assert is_binary(out)
      assert micros < 1_500_000, "#{div(micros, 1000)} ms for #{inspect(String.slice(text, 0, 24))}..."
    end
  end

  test "the slow text is still redacted as the pattern redacts it" do
    for text <- [
          String.duplicate("pwd-", 300) <> "=hunter2",
          String.duplicate("pwd-", 5) <> ":, pwd=x",
          String.duplicate("a-", 200) <> "token: \"abc def\""
        ] do
      assert by_hand(text) == by_pattern(text)
    end
  end
end
