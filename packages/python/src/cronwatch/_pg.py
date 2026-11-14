"""Opening a psycopg connection from a connection string without letting
the string's secrets into an error: libpq quotes the part it could not read,
which for a password with a stray "%" is the password itself, and that error
goes to on_error and the "cronwatch" logger on every check and every run."""

from __future__ import annotations

import os
import re
from typing import Any
from urllib.parse import urlsplit

__all__ = ["CONNECT_TIMEOUT_S", "KEEPALIVES", "TCP_USER_TIMEOUT_MS", "connect", "with_timeouts"]

#: Seconds a connection may take to open, when neither the connection string nor PGCONNECT_TIMEOUT says.
CONNECT_TIMEOUT_S = 10
#: TCP keepalive, when the connection string sets none of these: a peer that
#: went away without a word (a failover, a dropped NAT entry) is found after
#: about a minute of silence, where the system's default is two hours or more.
KEEPALIVES = {"keepalives": 1, "keepalives_idle": 30, "keepalives_interval": 10, "keepalives_count": 3}
#: Milliseconds sent data may go unacknowledged before the connection is given
#: up (Linux; libpq 12 or newer), when the connection string does not say.
TCP_USER_TIMEOUT_MS = 30_000


def with_timeouts(psycopg: Any, conninfo: str, statement_timeout_ms: int | None = None) -> str:
    """`conninfo` with the timeouts the store's own connection gets, each
    only where the app set none: a connection that opens slowly, or goes
    silent, would otherwise hold a job's start (written before the job runs)
    for as long as the kernel waits, minutes at least. All of them are the
    client's own settings, never sent to the server, so a pooler in front
    of it sees nothing new. `statement_timeout_ms`, when given, is the one
    that is sent: the server's statement_timeout, as a startup option, added
    to any options already there unless they set it. A string that cannot be
    read is handed back as it is, for connect() to refuse."""
    try:
        given = psycopg.conninfo.conninfo_to_dict(conninfo)
    except Exception:  # noqa: BLE001, connect() reports it without quoting it
        return conninfo
    added: dict[str, Any] = {}
    if "connect_timeout" not in given and not os.environ.get("PGCONNECT_TIMEOUT"):
        added["connect_timeout"] = CONNECT_TIMEOUT_S
    if not any(key in given for key in KEEPALIVES):
        added.update(KEEPALIVES)
    if "tcp_user_timeout" not in given and _libpq_version(psycopg) >= 120000:
        added["tcp_user_timeout"] = TCP_USER_TIMEOUT_MS
    if statement_timeout_ms is not None:
        options = given.get("options") or os.environ.get("PGOPTIONS") or ""
        if "statement_timeout" not in str(options):
            added["options"] = f"{options} -c statement_timeout={int(statement_timeout_ms)}".strip()
    if not added:
        return conninfo
    made: str = psycopg.conninfo.make_conninfo(conninfo, **added)
    return made


def _libpq_version(psycopg: Any) -> int:
    try:
        return int(psycopg.pq.version())
    except Exception:  # noqa: BLE001
        return 0


def connect(psycopg: Any, conninfo: str, **kwargs: Any) -> Any:
    """psycopg.connect(conninfo, **kwargs). A string psycopg cannot read raises
    an error naming only its host; an error connecting keeps libpq's text with
    the password, if it shows there, blanked."""
    try:
        psycopg.conninfo.conninfo_to_dict(conninfo)
    except Exception:  # noqa: BLE001, libpq's text quotes the token it could not read
        raise psycopg.ProgrammingError(
            f"cronwatch: the Postgres connection string for {_host(conninfo)} could not be read; "
            "check that any % in its user name or password is written as %25"
        ) from None
    try:
        return psycopg.connect(conninfo, **kwargs)
    except psycopg.Error as error:
        password = _password(psycopg, conninfo)
        text = str(error)
        if password and password in text:
            raise type(error)(text.replace(password, "***")) from None
        raise


def _host(conninfo: str) -> str:
    """The host a connection string names, as well as it can be read, for an error."""
    host: str | None = None
    try:
        if "://" in conninfo:
            host = urlsplit(conninfo).hostname
        else:
            match = re.search(r"(?:^|\s)host\s*=\s*'?([^\s']+)", conninfo)
            host = match.group(1) if match else None
    except ValueError:
        host = None
    return f'"{host}"' if host else "the default host"


def _password(psycopg: Any, conninfo: str) -> str | None:
    try:
        value = psycopg.conninfo.conninfo_to_dict(conninfo).get("password")
    except Exception:  # noqa: BLE001
        return None
    return value if isinstance(value, str) and value else None
