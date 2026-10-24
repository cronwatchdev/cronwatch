"""Slack, through an incoming webhook (alerts/slack.ts)."""

from __future__ import annotations

from collections.abc import Callable
from typing import Any

from .. import _js
from .._deprecated import names as _deprecated_names
from ..types import Alert
from ._http import HTTP
from ._shared import cut, http_or_default, link_for, positional_url, present, webhook_error

__all__ = ["Slack"]

_EMOJI = {
    "missed": ":hourglass_flowing_sand:",
    "failed": ":x:",
    "stuck": ":no_entry:",
    "slow": ":turtle:",
    "over_budget": ":moneybag:",
    "under_floor": ":chart_with_downwards_trend:",
    "recovered": ":white_check_mark:",
}


class Slack:
    """Sends alerts to a Slack channel through an incoming webhook
    (api.slack.com/messaging/webhooks)::

        Slack(webhook_url=os.environ["SLACK_WEBHOOK_URL"],
              link=lambda alert: f"https://app.example.com/cronwatch/jobs/{urllib.parse.quote(alert.job, safe='')}")

    Any ``<``, ``>``, or ``|`` in the link is percent-encoded, so it cannot
    end Slack's link early. The URL is taken by name; ``Slack(url)``, positionally, is deprecated (it
    warns, and goes in 2.0).
    """

    name = "slack"

    def __init__(
        self,
        _url: str | None = None,
        /,
        *,
        webhook_url: str | None = None,
        link: Callable[[Alert], Any] | None = None,
        http: HTTP | None = None,
    ) -> None:
        webhook_url = positional_url("Slack", _url, webhook_url)
        if not present(webhook_url):
            raise ValueError("Slack() needs a webhook_url")
        self._webhook_url = str(webhook_url)
        self._link = link
        self._http = http_or_default(http)

    def send(self, alert: Alert, context: Any = None) -> None:
        url = link_for(self._link, alert)
        title = f"{_EMOJI[str(alert.type)]} *{_escape(alert.title)}*{f' (<{_link_target(url)}|open>)' if url else ''}"
        body = _cut_escaped(_code_block_safe(_escape(alert.message)), 2900)
        blocks: list[dict[str, Any]] = [
            {"type": "section", "text": {"type": "mrkdwn", "text": title}},
            {"type": "section", "text": {"type": "mrkdwn", "text": "```" + body + "```"}},
        ]
        if present(alert.triage):
            # Its own block, so a long diagnosis cannot push a block past Slack's 3000 character limit.
            triage = _cut_escaped(f"_Triage:_ {_escape(str(alert.triage))}", 3000)
            blocks.append({"type": "section", "text": {"type": "mrkdwn", "text": triage}})
        payload = {
            # The notification fallback is parsed as mrkdwn too, so it is escaped like the blocks.
            "text": _escape(f"{alert.title}\n{alert.message}"),
            "blocks": blocks,
        }
        # Refused, not followed: a webhook URL is its own credential.
        response = self._http.post(self._webhook_url, _js.dumps(payload), {"content-type": "application/json"})
        if not response.ok:
            raise webhook_error("Slack", response.status, response.body, self._webhook_url)


def _escape(text: str) -> str:
    """Slack's three control characters. Escaping < and > also stops <!channel> and <url|links>."""
    return text.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def _cut_escaped(text: str, max_units: int) -> str:
    """Escaped text cut to at most `max_units` UTF-16 units, splitting neither an entity _escape wrote nor a surrogate pair."""
    kept = cut(text, max_units)
    amp = kept.rfind("&")
    return kept[:amp] if amp != -1 and kept.find(";", amp) == -1 else kept


def _link_target(url: str) -> str:
    """A link's URL as Slack's <url|text> can hold it: ``<``, ``>``, and ``|`` percent-encoded, which ends neither the link nor its text."""
    return url.replace("<", "%3C").replace(">", "%3E").replace("|", "%7C")


def _code_block_safe(text: str) -> str:
    """Breaks up ``` so text inside a code block cannot close it."""
    return text.replace("```", "`​`​`")


#: Internal names, still answering under their old public names (each
#: warning, until 1.0 removes them).
__getattr__ = _deprecated_names(__name__, globals(), {"EMOJI": "_EMOJI", "escape": "_escape", "code_block_safe": "_code_block_safe"})
