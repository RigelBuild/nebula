"""RIG-11725: secret-word query params on DB and JDBC URLs are masked by default.

``jdbc:postgresql://db:5432/app?user=u&password=V`` and ``redis://h:6379/0?passwd=V`` carry the
credential in the query string. The web-URL pass-through (TestWebUrlsNotRedacted) must not reach
them, so this pass covers DB and JDBC schemes only. http, https, wss and ftp query params stay
opt-in via ``redact_url_credentials=True``.
"""

import time

import pytest

from agent.redact import _redact_db_url_query_params, redact_sensitive_text

SECRET = "Zq8xR2vT9pL4mN7k"

SECRET_WORD_KEYS = [
    "password",
    "passwd",
    "pwd",
    "secret",
    "token",
    "key",
    "apikey",
    "api_key",
    "access_token",
    "client_secret",
    "sig",
    "signature",
]


@pytest.fixture(autouse=True)
def _ensure_redaction_enabled(monkeypatch):
    monkeypatch.delenv("HERMES_REDACT_SECRETS", raising=False)
    monkeypatch.setattr("agent.redact._REDACT_ENABLED", True)


class TestDbUrlSecretQueryParams:
    @pytest.mark.parametrize(
        ("text", "expected"),
        [
            (
                f"jdbc:postgresql://db:5432/app?user=u&password={SECRET}",
                "jdbc:postgresql://db:5432/app?user=u&password=***",
            ),
            (
                f"jdbc:postgresql://db:5432/app?user=u&Password={SECRET}",
                "jdbc:postgresql://db:5432/app?user=u&Password=***",
            ),
            (
                f"jdbc:mysql://db/app?client_secret={SECRET}",
                "jdbc:mysql://db/app?client_secret=***",
            ),
            (
                f"redis://h:6379/0?passwd={SECRET}",
                "redis://h:6379/0?passwd=***",
            ),
            (
                f"JDBC_URL=jdbc:postgresql://db:5432/app?sslmode=require&password={SECRET}",
                "JDBC_URL=jdbc:postgresql://db:5432/app?sslmode=require&password=***",
            ),
        ],
    )
    def test_secret_param_masked_key_kept(self, text, expected):
        result = redact_sensitive_text(text)
        assert SECRET not in result
        assert result == expected

    @pytest.mark.parametrize("key", SECRET_WORD_KEYS)
    def test_every_secret_word_key_is_masked(self, key):
        text = f"jdbc:postgresql://db/app?sslmode=require&{key}={SECRET}"
        assert (
            redact_sensitive_text(text)
            == f"jdbc:postgresql://db/app?sslmode=require&{key}=***"
        )

    @pytest.mark.parametrize(
        "key", ["Password", "PASSWORD", "Client_Secret", "API_KEY"]
    )
    def test_key_match_is_case_insensitive(self, key):
        text = f"jdbc:postgresql://db/app?{key}={SECRET}"
        assert redact_sensitive_text(text) == f"jdbc:postgresql://db/app?{key}=***"

    @pytest.mark.parametrize(
        "text",
        [
            "jdbc:postgresql://db/app?page=2&sort=asc",
            # Exact key match: ``token_type`` is not the secret word ``token``.
            "jdbc:postgresql://db/app?token_type=bearer",
            "jdbc:postgresql://db/app?keyspace=prod&monkey=1&passwordless=1",
            "jdbc:postgresql://db/app",
        ],
    )
    def test_non_secret_params_pass_through(self, text):
        assert redact_sensitive_text(text) == text

    def test_web_url_secret_param_passes_through_by_default(self):
        """Web URLs keep the pass-through contract (magic links, OAuth callbacks)."""
        text = f"https://idp/token?client_secret={SECRET}"
        assert redact_sensitive_text(text) == text
        assert redact_sensitive_text(text, redact_url_credentials=True) == (
            "https://idp/token?client_secret=***"
        )


class TestDbUrlQueryScaling:
    """The pass must stay linear: a 10x larger input may take at most 25x as long."""

    @staticmethod
    def _best_of(text: str) -> float:
        times = []
        for _ in range(3):
            t0 = time.perf_counter()
            _redact_db_url_query_params(text)
            times.append(time.perf_counter() - t0)
        return min(times)

    @pytest.mark.parametrize(
        "build",
        [
            # Many query params on one URL.
            lambda n: "jdbc:postgresql://db/app?" + "a=1&" * (n // 4),
            # One long token that repeats the scheme and ends in a query: the scheme
            # restarts must not rescan the token.
            lambda n: "postgresql://" * (n // 13) + "?password=" + SECRET,
        ],
    )
    def test_time_scales_linearly(self, build):
        small = self._best_of(build(100_000))
        large = self._best_of(build(1_000_000))
        assert large / small < 25, f"100 KB took {small:.4f}s, 1 MB took {large:.4f}s"
