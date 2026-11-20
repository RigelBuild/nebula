"""RIG-11549: URL paths are credentials (MCP server URLs, HA MCP ``/api/mcp/<secret>``).

Every log line must keep the scheme, host and port, and lose the path and query.
The redaction lives in ``RedactingFormatter`` so every Hermes handler gets it.
"""

import logging

import pytest

from agent.redact import RedactingFormatter

HA_SECRET = "Xq7mP2vK9wLr4tNb8cYd"
HA_URL = f"https://ha.example:8123/api/mcp/{HA_SECRET}"


@pytest.fixture(autouse=True)
def _ensure_redaction_enabled(monkeypatch):
    monkeypatch.delenv("HERMES_REDACT_SECRETS", raising=False)
    monkeypatch.setattr("agent.redact._REDACT_ENABLED", True)


def _format(name: str, msg: str, args: tuple = ()) -> str:
    record = logging.LogRecord(
        name=name, level=logging.INFO, pathname="", lineno=0,
        msg=msg, args=args, exc_info=None,
    )
    return RedactingFormatter("%(name)s: %(message)s").format(record)


def test_httpx2_request_line_logs_host_without_secret_path():
    # httpx2/_client.py logs 'HTTP Request: %s %s "%s %d %s"' with the full URL as an arg.
    line = _format(
        "httpx2",
        'HTTP Request: %s %s "%s %d %s"',
        ("POST", HA_URL, "HTTP/1.1", 200, "OK"),
    )
    assert HA_SECRET not in line
    assert "ha.example:8123" in line
    assert "https://ha.example:8123/<redacted>" in line


def test_httpx_request_line_logs_host_without_query_secret():
    secret = "tok-QWERTY1234567890"
    url = f"https://mcp.example.com/sse/v1?access_key={secret}"
    line = _format("httpx", "HTTP Request: GET %s", (url,))
    assert secret not in line
    assert "mcp.example.com" in line


def test_mcp_connect_failure_logs_ha_host_without_secret_path():
    # mcp_tool_discovery logs the server failure text with the URL in it.
    from tools.mcp_tool_discovery import logger as discovery_logger

    message = f"Failed to connect to MCP server 'ha' (url={HA_URL}): ConnectError"
    formatted = _format(discovery_logger.name, "%s", (message,))
    assert HA_SECRET not in formatted
    assert "ha.example:8123" in formatted


def test_plain_text_without_urls_is_unchanged():
    line = _format("tools.mcp_tool", "MCP server '%s' connected in %.1fs", ("ha", 1.5))
    assert line == "tools.mcp_tool: MCP server 'ha' connected in 1.5s"


@pytest.mark.parametrize(
    "url, secret",
    [
        ("https://h.example/sse?fields=a,b&access_token=S3CRETtokenVALUE99", "S3CRETtokenVALUE99"),
        ("https://h.example/sse?filter=(x)&key=PARENSECRET99", "PARENSECRET99"),
        ("https://h.example/api/mcp/abc,COMMASECRET99", "COMMASECRET99"),
        ("https://h.example/a)BRACKETSECRET99", "BRACKETSECRET99"),
        ("https://h.example/a]BRACKETSECRET99", "BRACKETSECRET99"),
        ("https://h.example/a'QUOTESECRET99", "QUOTESECRET99"),
    ],
)
def test_path_or_query_with_delimiter_char_loses_secret(url, secret):
    line = _format("httpx2", "HTTP Request: GET %s", (url,))
    assert secret not in line
    assert "h.example" in line


def test_wrapped_url_keeps_closing_delimiter():
    line = _format("httpx2", "see (%s) now", ("https://h.example/api/SECRETPATH99",))
    assert "SECRETPATH99" not in line
    assert line.endswith("(https://h.example/<redacted>) now")


def test_websocket_url_path_redacted():
    line = _format("mcp", "connecting %s", ("wss://ha.example:8123/api/mcp/WSSECRET99",))
    assert "WSSECRET99" not in line
    assert "wss://ha.example:8123/<redacted>" in line
