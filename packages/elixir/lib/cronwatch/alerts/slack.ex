defmodule Cronwatch.Alerts.Slack do
  @moduledoc """
  Sends alerts to a Slack channel through an incoming webhook
  (`alerts/slack.ts`):

      {Cronwatch.Alerts.Slack, webhook_url: System.fetch_env!("SLACK_WEBHOOK_URL")}

  Options:

    * `:webhook_url` (required): an incoming webhook URL from
      api.slack.com/messaging/webhooks. It is its own credential: errors
      never quote it, and a redirect is refused rather than followed.
    * `:link`: a function of the alert answering a link back to the job in
      your dashboard, such as `fn alert ->
      "https://app.example.com/cronwatch/jobs/\#{URI.encode_www_form_component(alert.job)}" end`.
      Any `<`, `>`, or `|` in it is percent-encoded, so it cannot end
      Slack's link early.
    * `:transport`: a `Cronwatch.Transport` for the request, else the
      instance's, else `Cronwatch.Transport.HTTP`.

  The message goes in a code block cut to 2,900 characters, a triage in a
  block of its own cut to 3,000 (neither cut splits an escape or a character
  outside the Basic Multilingual Plane), and Slack's `&`, `<`, and `>` are escaped,
  so job output cannot mention `@channel` or add links.
  """

  @behaviour Cronwatch.Channel

  alias Cronwatch.Alert
  alias Cronwatch.Alerts.Post
  alias Cronwatch.Alerts.Shared
  alias Cronwatch.JS
  alias Cronwatch.JS.Object

  @derive {Inspect, except: [:webhook_url, :transport]}
  defstruct [:webhook_url, link: nil, transport: nil]

  @type t :: %__MODULE__{webhook_url: String.t(), link: (Alert.t() -> String.t()) | nil, transport: term()}

  @impl true
  def init(opts) do
    with {:ok, o} <- Shared.options(opts, __MODULE__, [:webhook_url, :link, :transport]),
         :ok <- Shared.required(o, :webhook_url, __MODULE__),
         :ok <- Shared.check_link(o, __MODULE__),
         :ok <- Cronwatch.Transport.check(o[:transport], inspect(__MODULE__)) do
      {:ok, struct(__MODULE__, o)}
    end
  end

  @impl true
  def name(_), do: "slack"

  @impl true
  def send(%__MODULE__{} = o, %Alert{} = alert, ctx) do
    link = Shared.link_for(o.link, alert)

    head =
      "#{emoji(alert.type)} *#{escape(alert.title)}*" <> if(link != "", do: " (<#{link_target(link)}|open>)", else: "")

    code = "```" <> cut_escaped(code_block_safe(escape(alert.message)), 2900) <> "```"
    blocks = [section(head), section(code)]
    triage = Shared.triage(alert)

    # Its own block, so a long diagnosis cannot push a block past Slack's
    # 3000 character limit.
    blocks = if triage != "", do: blocks ++ [section(cut_escaped("_Triage:_ " <> escape(triage), 3000))], else: blocks

    payload =
      Object.new([
        # The notification fallback is parsed as mrkdwn too, so it is escaped like the blocks.
        {"text", escape(alert.title <> "\n" <> alert.message)},
        {"blocks", blocks}
      ])

    transport = o.transport || (ctx && ctx.transport)
    headers = [{"content-type", "application/json"}]

    # A redirect is refused, not followed: a webhook URL is its own credential.
    with {:ok, answer} <- Post.fetch(transport, Post.timeout(), o.webhook_url, headers, JS.stringify(payload)) do
      if Post.ok?(answer),
        do: :ok,
        # The start of the answer, with the URL's credential cut out in case a proxy echoes the request.
        else:
          {:error,
           Post.fail(
             "Slack webhook answered #{answer.status}: #{Post.error_body(answer.body, Post.url_secrets(o.webhook_url))}"
           )}
    end
  end

  defp section(text), do: Object.new([{"type", "section"}, {"text", Object.new([{"type", "mrkdwn"}, {"text", text}])}])

  defp emoji("missed"), do: ":hourglass_flowing_sand:"
  defp emoji("failed"), do: ":x:"
  defp emoji("stuck"), do: ":no_entry:"
  defp emoji("slow"), do: ":turtle:"
  defp emoji("over_budget"), do: ":moneybag:"
  defp emoji("under_floor"), do: ":chart_with_downwards_trend:"
  defp emoji("recovered"), do: ":white_check_mark:"
  defp emoji(_), do: ""

  # Slack's three control characters. Escaping < and > also stops
  # <!channel> and <url|links>.
  defp escape(text),
    do: text |> String.replace("&", "&amp;") |> String.replace("<", "&lt;") |> String.replace(">", "&gt;")

  # Escaped text cut to at most `max` UTF-16 units, splitting neither an
  # entity escape/1 wrote nor a surrogate pair.
  defp cut_escaped(text, max) do
    kept = Post.cut(text, max)

    case :binary.matches(kept, "&") |> List.last() do
      {at, _} ->
        if String.contains?(binary_part(kept, at, byte_size(kept) - at), ";"), do: kept, else: binary_part(kept, 0, at)

      nil ->
        kept
    end
  end

  # A link's URL as Slack's <url|text> can hold it: `<`, `>`, and `|`
  # percent-encoded, which ends neither the link nor its text.
  defp link_target(url),
    do: url |> String.replace("<", "%3C") |> String.replace(">", "%3E") |> String.replace("|", "%7C")

  # ``` with a zero width space (U+200B) between the ticks.
  @zwsp <<0x200B::utf8>>
  @broken "`" <> @zwsp <> "`" <> @zwsp <> "`"

  @doc false
  def broken, do: @broken

  @doc false
  # Breaks up ``` so text inside a code block cannot close it.
  def code_block_safe(text), do: String.replace(text, "```", @broken)
end
