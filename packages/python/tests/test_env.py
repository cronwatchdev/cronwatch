"""The environment, as conformance/client.json has every library read it, with ENVIRONMENT in
NODE_ENV's place: CRONWATCH_ENV, then APP_ENV, then ENVIRONMENT, the first
that holds more than spaces, trimmed, lowercased, and its alias resolved."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from cronwatch import _env

# conformance/client.json's environment table, with ENVIRONMENT as the native variable.
CLIENT = json.loads((Path(__file__).resolve().parents[3] / "conformance" / "client.json").read_text("utf-8"))
CASES = [(c["CRONWATCH_ENV"], c["APP_ENV"], c["native"], c["environment"] or None) for c in CLIENT["environment"]]


@pytest.fixture(autouse=True)
def _clean(monkeypatch: pytest.MonkeyPatch) -> None:
    for name in _env._VARIABLES:
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setattr(_env, "_fallback", None)


@pytest.mark.parametrize(("cronwatch_env", "app_env", "own", "expected"), CASES)
def test_the_first_variable_with_a_value_names_the_environment(
    monkeypatch: pytest.MonkeyPatch, cronwatch_env: str | None, app_env: str | None, own: str | None, expected: str | None
) -> None:
    for name, value in zip(_env._VARIABLES, (cronwatch_env, app_env, own), strict=True):
        if value is not None:
            monkeypatch.setenv(name, value)
    assert _env.environment() == expected
    assert _env.is_development() is (expected == "development")
    assert _env.is_production() is (expected == "production")


def test_a_framework_names_it_only_when_no_variable_does(monkeypatch: pytest.MonkeyPatch) -> None:
    _env.set_fallback(lambda: "development")
    assert _env.is_development()
    monkeypatch.setenv("APP_ENV", "prod")
    assert _env.is_production()
    monkeypatch.setenv("APP_ENV", "  ")
    assert _env.is_development()
    _env.set_fallback(lambda: " ")
    assert _env.environment() is None


@pytest.mark.parametrize("case", CLIENT["secrets"], ids=lambda c: repr(c["value"]))
def test_secrets_are_trimmed_and_blank_ones_unset(monkeypatch: pytest.MonkeyPatch, case: dict[str, str | None]) -> None:
    monkeypatch.setenv("CRON_SECRET", case["value"])
    assert _env.read_secret_env("CRON_SECRET") == case["secret"]
    unset = object()
    given = _env.secret_option(case["value"], "token", unset)
    assert (None if given is unset else given) == case["secret"]
