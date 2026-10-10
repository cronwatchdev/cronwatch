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

import os
from collections.abc import Callable

from . import _js

__all__ = ["environment", "is_development", "is_production", "read_secret_env", "secret_option", "set_fallback"]

_VARIABLES = ("CRONWATCH_ENV", "APP_ENV", "ENVIRONMENT")
_ALIASES = {"prod": "production", "dev": "development", "local": "development"}
_fallback: Callable[[], str | None] | None = None


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
    """A token or secret passed in code: a str, None (the opt-out), or `unset`
    (not given). A str is trimmed, as read_secret_env() trims, and one that is
    empty or only whitespace counts as not given (`unset` comes back).
    Anything else (False, a number, bytes) raises a TypeError naming the
    option, so it never becomes a password."""
    if value is unset or value is None:
        return value
    if not isinstance(value, str):
        raise TypeError(f"{what} must be a string, or None to opt out, not {type(value).__name__}")
    return unset if _blank(value) else _js.trim(value)


def is_production() -> bool:
    return environment() == "production"


def is_development() -> bool:
    return environment() == "development"
