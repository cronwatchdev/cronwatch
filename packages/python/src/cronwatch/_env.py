"""The environment, read in one place, as every CronWatch library reads it:
the first of CRONWATCH_ENV, APP_ENV, and ENVIRONMENT that holds more than
whitespace, trimmed and lowercased, with "prod" read as "production" and
"dev" and "local" as "development"; any other value as written, so "test"
and "testing" are not development (a test run or CI needs CRON_SECRET or a
token, as production does). Failing those, a framework
integration may name one (cronwatch.django reads DEBUG: development when it
is on, production when off). None when nothing names one, which is neither.
It decides whether the in-memory store warns that it forgets on restart, and
whether the web routes make a development token when none is configured."""

from __future__ import annotations

import logging
import os
import threading
from collections.abc import Callable

from . import _js

__all__ = ["OPEN", "environment", "is_development", "is_production", "is_open", "read_secret_env", "secret_option", "set_fallback", "warn_open"]

_VARIABLES = ("CRONWATCH_ENV", "APP_ENV", "ENVIRONMENT")
_ALIASES = {"prod": "production", "dev": "development", "local": "development"}
_fallback: Callable[[], str | None] | None = None
_log = logging.getLogger("cronwatch")


class _Open:
    """The type of OPEN, which has one value."""

    __slots__ = ()

    def __repr__(self) -> str:
        return "cronwatch.OPEN"


#: Passed as ``token=``, ``cron_secret=``, or ``secret=``: serve the routes, or
#: run a handler, for anyone, with no token or secret, on purpose (behind the
#: app's own auth, say). None means the same and still works, but it is also
#: what ``os.environ.get("CRONWATCH_TOKEN")`` gives for a variable that is not
#: set, so a None outside development is warned about (warn_open) and OPEN is not.
OPEN = _Open()

_warned_open: set[str] = set()
_warned_open_lock = threading.Lock()


def is_open(value: object) -> bool:
    """Whether a token or secret option opts out: OPEN, or None."""
    return value is None or value is OPEN


def warn_open(value: object, what: str, name: str, variable: str) -> None:
    """Warns, once per process for each `what`, that `what` (the dashboard's
    routes, job handlers) is open to anyone because `name` was None, outside
    development. OPEN, the opt-out by name, does not warn."""
    if value is not None or is_development():
        return
    with _warned_open_lock:
        if what in _warned_open:
            return
        _warned_open.add(what)
    _log.warning(
        "%s open to anyone, with no %s asked for, because %s is None. If that is meant, pass cronwatch.OPEN, which says so and "
        'does not warn. If it came from os.environ.get("%s") with the variable not set, leave %s out (the variable is read by '
        'default, and without it nothing is served) or give the lookup a default of "".',
        what,
        "token" if "token" in name.lower() else "secret",
        name,
        variable,
        name,
    )


def set_fallback(read: Callable[[], str | None] | None) -> None:
    """What names the environment when no variable does (a framework's own setting), or None for nothing."""
    global _fallback
    _fallback = read


def _name(value: str | None) -> str | None:
    """A value trimmed as JavaScript trims and lowercased, its alias resolved; None for one of only spaces."""
    name = _js.trim(value).lower() if isinstance(value, str) else ""
    return _ALIASES.get(name, name) or None


def environment() -> str | None:
    for variable in _VARIABLES:
        name = _name(os.environ.get(variable))
        if name is not None:
            return name
    read = _fallback
    if read is not None:
        try:
            return _name(read())
        except Exception:
            return None
    return None


def _blank(value: str) -> bool:
    """Empty, or only whitespace as JavaScript's String.prototype.trim sees it
    (not str.strip(), which also strips U+001C to U+001F and U+0085 and keeps U+FEFF)."""
    return _js.trim(value) == ""


def read_secret_env(name: str) -> str | None:
    """A secret from the environment (CRONWATCH_TOKEN, CRON_SECRET), trimmed as
    JavaScript trims: a value read from a file often ends in a newline that no
    header can carry, since HTTP strips the whitespace around a header's
    value. None when the variable is unset, empty, or only whitespace, so a
    blank value counts as not set and the routes and handlers fail closed."""
    value = os.environ.get(name)
    return None if value is None or _blank(value) else _js.trim(value)


def secret_option(value: object, what: str, unset: object) -> object:
    """A token or secret passed in code: a str, OPEN or None (the opt-out,
    which comes back as None), or `unset` (not given). A str is trimmed, as read_secret_env() trims, and one that is
    empty or only whitespace counts as not given (`unset` comes back).
    Anything else (False, a number, bytes) raises a TypeError naming the
    option, so it never becomes a password."""
    if value is unset or value is None:
        return value
    if value is OPEN:
        return None
    if not isinstance(value, str):
        raise TypeError(f"{what} must be a string, or None to opt out, not {type(value).__name__}")
    return unset if _blank(value) else _js.trim(value)


def is_production() -> bool:
    return environment() == "production"


def is_development() -> bool:
    return environment() == "development"
