"""Round-3 review rows for the Kubernetes pass (RIG-11725): go-template/jsonpath Secret reads,
``kubectl set env --list``, Go-map values with brackets or ``word:`` inside, and JSON nesting
past the depth cap."""

import pytest

from agent.redact import redact_sensitive_text as r, redact_terminal_output

V = "zzqq" + "-fixture-leak-value-77"
K = "keep-visible-r5"


@pytest.fixture(autouse=True)
def _ensure_redaction_enabled(monkeypatch):
    monkeypatch.delenv("HERMES_REDACT_SECRETS", raising=False)
    monkeypatch.setattr("agent.redact._REDACT_ENABLED", True)


def _terminal(text, command):
    return [redact_terminal_output(text, command, force=True), redact_terminal_output(text, command)]


def _plain(text):
    return [r(text, force=True), r(text)]


# --- H1: go-template / jsonpath reads of Secrets -----------------------------------------------

def test_h1_go_template_secret_read():
    text = V + "\n"
    command = "kubectl get secret s -o go-template='{{.data.k | base64decode}}'"
    for out in _terminal(text, command):
        assert V not in out, out


def test_h1_jsonpath_secret_read():
    text = V + "\n"
    command = "kubectl get secret s -o jsonpath='{.data.k}'"
    for out in _terminal(text, command):
        assert V not in out, out


def test_h1_jsonpath_secrets_list():
    text = '{"k":"' + V + '"}\n'
    command = "kubectl get secrets -n ns -o jsonpath='{.items[*].data}'"
    for out in _terminal(text, command):
        assert V not in out, out


def test_h1_go_template_attached_flag():
    text = V + "\n"
    command = "kubectl get secret/s --output=go-template={{.data.k}}"
    for out in _terminal(text, command):
        assert V not in out, out


def test_h1_jsonpath_pods_kept():
    text = "web-1\n"
    command = "kubectl get pods -o jsonpath='{.items[*].metadata.name}'"
    for out in _terminal(text, command):
        assert "web-1" in out, out


# --- H2: kubectl set env --list prints NAME=value ----------------------------------------------

def test_h2_set_env_list_plain_name():
    text = "# Deployment web, container app\nLOG_DEST=" + V + "\nDB_HOST=db\n"
    command = "kubectl set env deployment/web --list"
    for out in _terminal(text, command):
        assert V not in out, out


def test_h2_set_env_list_secret_name():
    text = "DB_PASSWORD=" + V + "\n"
    command = "kubectl set env deploy/web -c app -l"
    for out in _terminal(text, command):
        assert V not in out, out


# --- H3: Go-map values cut off early -----------------------------------------------------------

def test_h3_gomap_bracket_in_value():
    text = "[map[name:DB_PASSWORD value:ab] " + V + "]"
    for out in _plain(text):
        assert V not in out, out


def test_h3_gomap_word_colon_in_value():
    text = "[map[name:DB_PASSWORD value:ab cd:" + V + "]]"
    for out in _plain(text):
        assert V not in out, out


def test_h3_gomap_reversed_bracket_in_value():
    text = "[map[value:ab] " + V + " name:DB_PASSWORD]]"
    for out in _plain(text):
        assert V not in out, out


def test_h3_gomap_other_map_kept():
    text = "[map[name:LOG value:info] map[name:DB_PASSWORD value:x]]"
    for out in _plain(text):
        assert "info" in out, out


# --- M1: JSON nesting past _MAX_DEPTH ----------------------------------------------------------

def test_m1_secret_data_past_depth():
    text = ('{"apiVersion":"v1","kind":"Secret","pad":' + "[" * 520
            + '{"data":{"k":"' + V + '"}}' + "]" * 520 + "}")
    out = r(text, force=True)
    assert V not in out, out[:200]


def test_m1_secret_data_past_depth_600_prefix():
    text = (' {"apiVersion":"v1","kind":"Secret","pad":' + "[" * 600
            + '{"data":{"k":"' + V + '"}}' + "]" * 600 + "}")
    out = r(text, force=True)
    assert V not in out, out[:200]
