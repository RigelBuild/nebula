"""Header redaction must preserve the syntax consumed by diagnostic tools."""

import json
import shlex
import time

from agent.redact import redact_sensitive_text


def test_json_headers_remain_parseable_without_exposing_credentials():
    for name in ("x-api-key", "X-Goog-Api-Key", "apikey", "Cookie", "Set-Cookie"):
        for value in ('abcd="opaque-session-secret-1234567890"; Path=/',
                      's=short"value', "opaque\\session\\credential1234567890"):
            payload = {"headers": {name: value}, "model": "example", "messages": []}
            serialized = json.dumps(payload)
            redacted = redact_sensitive_text(serialized, force=True)
            restored = json.loads(redacted)
            assert restored["headers"][name] != value
            assert "opaque-session-secret" not in restored["headers"][name]
            assert restored["model"] == payload["model"]
            assert restored["messages"] == payload["messages"]
            file_redacted = json.loads(redact_sensitive_text(serialized, force=True, file_read=True, secret_file=True))
            assert file_redacted["headers"][name].startswith("«redacted")
            assert redact_sensitive_text(serialized, force=True, code_file=True) == serialized
        if "cookie" not in name.lower():  # a slash-led opaque key is a credential, not a path
            slash_key = json.dumps({name: "/abcdef0123456789abcdef0123456789"})
            assert "0123456789abcdef0123" not in redact_sensitive_text(slash_key, force=True)
        vendor_key = json.dumps({name: "ghp_" + "A1b2C3d4" * 5})
        assert "«redacted:ghp_" in redact_sensitive_text(vendor_key, force=True, file_read=True, secret_file=True)


def test_raw_headers_mask_all_values_without_consuming_shell_or_next_line():
    value = "sid=opaque-session-secret-1234567890; second=another-credential"
    for name in ("Cookie", "sEt-CoOkIe", "x-api-key"):
        raw = f"{name}: {value}\nX-Trace: unchanged"
        assert redact_sensitive_text(raw, force=True) == f"{name}: ***\nX-Trace: unchanged"
        for flag in ("-H", "--header"):
            for quote in ("'", '"'):
                command = f"curl {flag} {quote}{name}: {value}{quote} https://example.com/path"
                output = redact_sensitive_text(command, force=True)
                assert shlex.split(output) == ["curl", flag, f"{name}: ***", "https://example.com/path"]
    for benign in ("the cookie: is a browser feature", 'Cookie.set("name", value)'):
        assert redact_sensitive_text(benign, force=True) == benign


def test_curl_cookie_options_and_csrf_headers_are_masked():
    for command in (
        "curl -b 'a=opaque-cookie-secret' https://example.com",
        "curl --cookie 'a=opaque-cookie-secret' https://example.com",
        "curl --cookie='a=opaque-cookie-secret' https://example.com",
        'curl -b "a=opaque-cookie-secret" https://example.com',
        'curl --cookie="a=opaque-cookie-secret" https://example.com',
        "curl -b a=opaque-cookie-secret https://example.com",
        "curl --cookie=a=opaque-cookie-secret https://example.com",
        "curl -bsession=opaque-cookie-secret https://example.com",
        'curl -b "a\\"opaque-cookie-secret" https://example.com',
    ):
        redacted = redact_sensitive_text(command, force=True, code_file=True)
        assert "opaque-cookie-secret" not in redacted
    for name in ("X-CSRF-Token", "X-XSRF-TOKEN", "X-CSRFToken"):
        text = f'{name}: opaque-csrf-secret\nX-Trace: unchanged'
        assert redact_sensitive_text(text, force=True) == f'{name}: ***\nX-Trace: unchanged'
        command = f'curl -H "{name}: opaque-csrf-secret" https://example.com'
        assert "opaque-csrf-secret" not in redact_sensitive_text(command, force=True)


def test_har_cookie_values_and_csrf_headers_are_masked_in_code_output():
    payload = {
        "request": {
            "cookies": [{"name": "session", "value": "opaque-har-cookie-secret"}],
            "headers": [{"name": "X-CSRF-Token", "value": "opaque-har-csrf-secret"}],
        },
        "response": {
            "cookies": [{"name": "session", "value": "opaque-har-set-cookie-secret"}],
            "headers": [{"name": "Set-Cookie", "value": "session=opaque-har-cookie-header-secret"}],
        },
    }
    redacted = json.loads(redact_sensitive_text(json.dumps(payload), force=True, code_file=True))
    assert redacted["request"]["cookies"][0] == {"name": "session", "value": "***"}
    assert redacted["response"]["cookies"][0] == {"name": "session", "value": "***"}
    assert redacted["request"]["headers"][0]["value"] == "***"
    assert redacted["response"]["headers"][0]["value"] == "***"


def test_cookie_options_do_not_mask_other_commands_or_cookie_jars():
    for command in (
        "git checkout -b feature/x", "grep -B 3", "tar -b 20", "cut -b 1-5",
        "[ -b /dev/sda ]", "curl -b cookies.txt", "curl https://x; git checkout -b feature/x",
    ):
        assert redact_sensitive_text(command, force=True, code_file=True) == command


def test_har_cookie_arrays_handle_nesting_and_truncation_in_linear_time():
    text = '{"cookies":[{"value":"secret-one","tags":[1]},{"value":"secret-two"}],"items":[{"value":"neutral"}]}'
    result = redact_sensitive_text(text, force=True, code_file=True)
    assert "secret-one" not in result and "secret-two" not in result
    assert '"value":"neutral"' in result
    truncated = '"cookies":[' + '"a"' * 5000 + '{"value":"secret"}'
    start = time.monotonic()
    result = redact_sensitive_text(truncated, force=True, code_file=True)
    assert time.monotonic() - start < 1
    assert '"value":"***"' in result
