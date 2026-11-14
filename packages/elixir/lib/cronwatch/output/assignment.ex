defmodule Cronwatch.Output.Assignment do
  @moduledoc false
  # Internal: not the package's API, and it can change in any release.
  #
  # The SDK's key=value redaction pattern (`Cronwatch.Output.assignment_source/0`,
  # flags `gi`), matched by hand over UTF-16 code units rather than by
  # `Cronwatch.JSRE`, with the same matches and so the same bytes.
  #
  # The pattern is a name of up to 40 characters, a keyword, up to 40 more,
  # then `"?\s{0,3}(?:=>|[=:])\s{0,3}` and a value. A backtracking engine
  # tries every length of the name against every length of what follows the
  # keyword, some 1,700 ways from each place a name can start, and a step
  # costs the BEAM far more than it costs V8: 68 KB of `pwd-pwd-pwd-` took
  # seconds. What it finds does not need the search. Everything from the
  # match's start to the separator is one run of `[A-Za-z0-9_-]` (the
  # keywords are made of those characters, and what follows the run must be
  # a quote, white space, or the separator, none of which is one), so:
  #
  #   * whether a run can end a match, and where the value then starts and
  #     ends, depends only on where the run ends, and is worked out once per
  #     run;
  #   * a match starts at the first place in the run that is a word boundary
  #     and has a keyword within 40 characters after it that ends within 40
  #     characters of the run's end.
  #
  # The replacement keeps everything up to the value, so which keyword and
  # which lengths the engine would have settled on change nothing. The
  # tests run this against the pattern itself in `Cronwatch.JSRE` over
  # generated text, and `conformance/output.json` holds both to the SDK.

  alias Cronwatch.JS

  @name_max 40
  @value_max 4096
  # The longest keyword: `private_key`.
  @keyword_max 11

  @doc "The text, as UTF-16 code units, with every match's value replaced by `redacted` (units too)."
  @spec replace(binary(), binary()) :: binary()
  def replace(units, redacted) do
    n = div(byte_size(units), 2)
    loop(units, n, redacted, 0, 0, [])
  end

  # `last` is where the text not yet written out starts, `pos` where the
  # next match may start.
  defp loop(u, n, redacted, last, pos, acc) do
    case next_run(u, n, pos) do
      nil ->
        if acc == [], do: u, else: IO.iodata_to_binary(Enum.reverse([slice(u, last, n) | acc]))

      {from, to} ->
        with {value, quote, finish} <- tail(u, n, to),
             true <- start_in?(u, n, from, to) do
          acc = [[slice(u, last, value), quote, redacted, quote] | acc]
          loop(u, n, redacted, finish, finish, acc)
        else
          _ -> loop(u, n, redacted, last, to, acc)
        end
    end
  end

  defp slice(u, from, to), do: binary_part(u, from * 2, (to - from) * 2)

  # The unit at `i`, or -1 outside the text.
  defp at(u, n, i) when i >= 0 and i < n do
    <<x::16>> = binary_part(u, i * 2, 2)
    x
  end

  defp at(_u, _n, _i), do: -1

  defp name?(c), do: c in ?a..?z or c in ?A..?Z or c in ?0..?9 or c == ?_ or c == ?-
  defp word?(c), do: c in ?a..?z or c in ?A..?Z or c in ?0..?9 or c == ?_
  defp lower(c) when c in ?A..?Z, do: c + 32
  defp lower(c), do: c

  # The next run of name characters at or after `pos`: where it starts (or
  # `pos`, inside one) and the place after its last character.
  defp next_run(u, n, pos) do
    case skip(u, n, pos, &(not name?(&1))) do
      ^n -> nil
      from -> {from, skip(u, n, from, &name?/1)}
    end
  end

  # The first place at or after `i` whose unit `fun` does not hold for.
  defp skip(_u, n, i, _fun) when i >= n, do: n

  defp skip(u, n, i, fun) do
    if fun.(at(u, n, i)), do: skip(u, n, i + 1, fun), else: i
  end

  # How many units from `i` on `fun` holds for, `max` at most.
  defp count(u, n, i, max, fun), do: count(u, n, i, max, fun, 0)
  defp count(_u, _n, _i, max, _fun, k) when k >= max, do: k

  defp count(u, n, i, max, fun, k) do
    c = at(u, n, i + k)
    if c >= 0 and fun.(c), do: count(u, n, i, max, fun, k + 1), else: k
  end

  ## What follows the run

  # `(?<![Tt][Oo][Kk][Ee][Nn][Ss])"?\s{0,3}(?:=>|[=:])\s{0,3}` and the
  # value, from the run's end: `{value_start, quote, match_end}`, or nil.
  # `=>` is tried before `=`, as the pattern writes them, so `key=>,` (no
  # value after `=>`) is `key=` and the value `>`.
  defp tail(u, n, to) do
    if tokens?(u, n, to) do
      nil
    else
      a = if at(u, n, to) == ?", do: to + 1, else: to

      with a when is_integer(a) <- spaces(u, n, a) do
        arrow = if at(u, n, a) == ?= and at(u, n, a + 1) == ?>, do: value(u, n, a + 2)
        arrow || if(at(u, n, a) in [?=, ?:], do: value(u, n, a + 1))
      end
    end
  end

  defp tokens?(u, n, to) do
    to >= 6 and Enum.map(-6..-1//1, &lower(at(u, n, to + &1))) == ~c"tokens"
  end

  # Past `\s{0,3}`, or nil when more white space follows than it takes:
  # nothing after it in the pattern starts with any.
  defp spaces(u, n, i) do
    case count(u, n, i, 4, &JS.space?/1) do
      4 -> nil
      k -> i + k
    end
  end

  # `\s{0,3}` and the value's three forms, in the pattern's order.
  defp value(u, n, i) do
    with v when is_integer(v) <- spaces(u, n, i) do
      quoted(u, n, v, ?") || quoted(u, n, v, ?') || bare(u, n, v)
    end
  end

  # `(")[^"\n]{1,4096}"`, and the same with `'`.
  defp quoted(u, n, v, q) do
    if at(u, n, v) == q do
      k = count(u, n, v + 1, @value_max, &(&1 != q and &1 != ?\n))
      if k >= 1 and at(u, n, v + 1 + k) == q, do: {v, <<q::16>>, v + k + 2}
    end
  end

  # `["']?[^\s"',;&]{1,4096}`.
  defp bare(u, n, v) do
    s = if at(u, n, v) in [?", ?'], do: v + 1, else: v

    case count(u, n, s, @value_max, &(not (JS.space?(&1) or &1 in [?", ?', ?,, ?;, ?&]))) do
      0 -> nil
      k -> {v, "", s + k}
    end
  end

  ## Where a match starts in the run

  # Whether some place in the run `from..to` starts a match: a word
  # boundary with a keyword at most 40 characters after it that ends at
  # most 40 characters before the run's end.
  defp start_in?(u, n, from, to) do
    keywords =
      for q <- max(from, to - @name_max - @keyword_max)..(to - 3)//1,
          len = keyword(u, n, q),
          len > 0 and q + len <= to and to - (q + len) <= @name_max,
          do: q

    case keywords do
      [] ->
        false

      [first | _] ->
        Enum.any?(max(from, first - @name_max)..List.last(keywords)//1, fn p ->
          boundary?(u, n, p) and Enum.any?(keywords, &(&1 >= p and &1 - p <= @name_max))
        end)
    end
  end

  defp boundary?(u, n, p), do: word?(at(u, n, p - 1)) != word?(at(u, n, p))

  # The length of the keyword at `q`, or 0. No two of them start alike, so
  # at most one matches, with one length.
  defp keyword(u, n, q) do
    case lower(at(u, n, q)) do
      ?s -> word(u, n, q, ~c"secret")
      ?t -> word(u, n, q, ~c"token")
      ?c -> word(u, n, q, ~c"credential")
      ?a -> max(joined(u, n, q, ~c"api"), joined(u, n, q, ~c"access"))
      ?p -> p_keyword(u, n, q)
      _ -> 0
    end
  end

  defp p_keyword(u, n, q) do
    Enum.max([
      word(u, n, q, ~c"pwd"),
      word(u, n, q, ~c"passwd"),
      word(u, n, q, ~c"password"),
      joined(u, n, q, ~c"private")
    ])
  end

  defp word(u, n, q, chars) do
    if chars |> Enum.with_index() |> Enum.all?(fn {c, i} -> lower(at(u, n, q + i)) == c end),
      do: length(chars),
      else: 0
  end

  # `<chars>[_-]?key`.
  defp joined(u, n, q, chars) do
    len = length(chars)

    cond do
      word(u, n, q, chars) == 0 -> 0
      word(u, n, q + len, ~c"key") > 0 -> len + 3
      at(u, n, q + len) in [?_, ?-] and word(u, n, q + len + 1, ~c"key") > 0 -> len + 4
      true -> 0
    end
  end
end
