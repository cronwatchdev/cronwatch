"""Any URL, the alert as JSON (alerts/webhook.ts)."""

from __future__ import annotations

import hashlib
import hmac
import time
import warnings
from collections.abc import Callable, Mapping
from typing import Any

from .. import _js
from ..types import Alert
from ._http import HTTP
from ._shared import http_or_default, origin, present


__all__ = ["WEBHOOK_SCHEMA", "Webhook", "signature"]

#: The payload's version, sent as its first field. It goes up only if a major
#: release changes the payload in a way that is not additive; see
#: https://cronwatch.dev/schemas/webhook/1.json.
WEBHOOK_SCHEMA = 1


class Webhook:
    """POSTs the alert as JSON to any URL. The body is the alert's JSON after
    a version: { schema: 1, type, run, details, job, definition, title,
    message, at, triage }.
    With a secret, each request is signed with it (HMAC-SHA256, as lowercase
    hex), so the receiver can verify it:

    - ``X-CronWatch-Signature: sha256=<hex>`` signs the raw body alone, as it
      always has.
    - ``X-CronWatch-Timestamp: <unix seconds>`` is when it was sent, and
      ``X-CronWatch-Timestamp-Signature: sha256=<hex>`` signs
      ``<timestamp>.<raw body>``, so a receiver that checks this one (with
      ``hmac.compare_digest``) and refuses a timestamp more than five minutes
      from its own clock cannot be sent a captured alert again later.

    An empty string (an unset variable read as "", say) signs nothing, and
    each send reports that to on_error. ``now`` is the clock the timestamp
    is read from, in epoch milliseconds. Header values are trimmed, and a
    header's name is matched without regard to case, so a Content-Type given
    in ``headers`` replaces the default one rather than going beside it; the
    signature headers are always the channel's own. A redirect is an error,
    not followed (the headers and the signature would go with it): point the
    url at where the receiver really is."""

    name = "webhook"

    def __init__(
        self,
        url: str,
        *,
        headers: Mapping[str, str] | None = None,
        secret: str | None = None,
        now: Callable[[], int] | None = None,
        http: HTTP | None = None,
    ) -> None:
        if not present(url):
            raise ValueError("Webhook() needs a url")
        self._url = str(url)
        self._headers = dict(headers or {})
        self._secret = secret
        self._now = now or (lambda: time.time_ns() // 1_000_000)
        self._http = http_or_default(http)

    def send(self, alert: Alert, context: Any = None) -> None:
        body = _js.dumps({"schema": WEBHOOK_SCHEMA, **alert.to_dict()})
        report = getattr(context, "on_error", None)
        if self._secret == "" and callable(report):
            report(RuntimeError("webhook: secret is an empty string, so this request is sent unsigned; set the secret, or leave it out to send unsigned on purpose"))
        headers = {"content-type": "application/json", "user-agent": "cronwatch"}
        # A pasted Authorization value often carries a stray space or newline, which a header would refuse.
        for name, value in self._headers.items():
            _set_header(headers, str(name), _js.trim(value) if isinstance(value, str) else value)
        if self._secret:
            timestamp = str(int(self._now() // 1000))
            _set_header(headers, "x-cronwatch-signature", f"sha256={signature(self._secret, body)}")
            _set_header(headers, "x-cronwatch-timestamp", timestamp)
            _set_header(headers, "x-cronwatch-timestamp-signature", f"sha256={signature(self._secret, f'{timestamp}.{body}')}")
        response = self._http.post(self._url, body, headers)
        # Only the origin: a webhook URL's path or query often is the credential.
        if not response.ok:
            raise RuntimeError(f"Webhook {origin(self._url)} answered {response.status}")


def _set_header(headers: dict[str, Any], name: str, value: Any) -> None:
    """Sets a header, first taking out any of the same name in another case."""
    lower = name.lower()
    for key in [key for key in headers if key.lower() == lower]:
        del headers[key]
    headers[name] = value


def signature(secret: str, body: str) -> str:
    """The webhook's signature of a body: the HMAC-SHA256 of the body with
    the secret, as lowercase hex. A request carries it as
    ``X-CronWatch-Signature: sha256=<signature>``, so a receiver checks one
    with the raw body it got (before any JSON parsing) and
    ``hmac.compare_digest``. The timestamped one is the signature of
    ``<timestamp>.<body>``, with the X-CronWatch-Timestamp value as sent.
    Every port has it, named the same."""
    return hmac.new(_js.well_formed(secret).encode("utf-8"), _js.well_formed(body).encode("utf-8"), hashlib.sha256).hexdigest()


def hmac_sha256_hex(secret: str, body: str) -> str:
    """Deprecated: use :func:`signature`, which this calls. Removed in 1.0."""
    warnings.warn("hmac_sha256_hex() is deprecated and removed in 1.0: use cronwatch.alerts.webhook.signature()", DeprecationWarning, stacklevel=2)
    return signature(secret, body)
