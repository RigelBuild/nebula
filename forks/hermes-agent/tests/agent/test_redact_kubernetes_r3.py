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


def test_h1_jsonpath_pods_intended_over_mask():
    text = "web-1\n"
    command = "kubectl get pods -o jsonpath='{.items[*].metadata.name}'"
    # intended over-mask: option A masks any -o jsonpath read.
    for out in _terminal(text, command):
        assert out == "***", out


# --- H2: kubectl set env --list prints NAME=value ----------------------------------------------

def test_h2_set_env_list_plain_name():
    text = "# Deployment web, container app\nLOG_DEST=" + V + "\nDB_HOST=db\n"
    command = "kubectl set env deployment/web --list"
    for out in _terminal(text, command):
        assert V not in out, out


def test_h2_set_env_list_secret_name_masked_by_name():
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


# --- R1: Secret reads through template-like output (not only go-template/jsonpath) -----------

@pytest.mark.parametrize("command", [
    "kubectl get secret s --template='{{.data.k}}'",
    "kubectl get secret s -o template='{{.data.k}}'",
    "kubectl get secret s -o custom-columns=K:.data.k",
    "kubectl get secret s -o custom-columns=:.data",
])
def test_r1_secret_template_like_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out


# --- R2: kubectl behind a wrapper, env prefix, sh -c or a .exe name ----------------------------

@pytest.mark.parametrize("command", [
    "KUBECONFIG=/tmp/c kubectl get secret s -o jsonpath='{.data.k}'",
    "sudo kubectl get secret s -o jsonpath='{.data.k}'",
    "watch kubectl get secret s -o jsonpath='{.data.k}'",
    'bash -c "kubectl get secret s -o jsonpath=\'{.data.k}\'"',
    "kubectl.exe get secret s -o jsonpath='{.data.k}'",
])
def test_r2_kubectl_behind_wrapper(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out


# --- R3: set env --list spelled long, and the .exe binary --------------------------------------

def test_r3_set_env_long_list_flag():
    text = "LOG_DEST=" + V + "\n"
    for out in _terminal(text, "kubectl set env deployment/web --list=true"):
        assert V not in out, out


def test_r3_set_env_list_exe_binary():
    text = "LOG_DEST=" + V + "\n"
    for out in _terminal(text, "kubectl.exe set env deploy/w --list"):
        assert V not in out, out


# --- R4: Secret resource forms with a group/version or a file input ----------------------------

@pytest.mark.parametrize("command", [
    "kubectl get secrets.v1 s -o jsonpath='{.data.k}'",
    "kubectl get secret.v1. s -o jsonpath='{.data.k}'",
    "kubectl get -f secret.yaml -o jsonpath='{.data.k}'",
])
def test_r4_secret_resource_forms(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out


# --- R5: Go-map value whose text holds "map[" then a key-like word -----------------------------

def test_r5_gomap_map_then_key_inside_value():
    text = "[map[name:DB_PASSWORD value:abmap[ " + V + "]]"
    for out in _plain(text):
        assert V not in out, out


# --- R6: a Go-map value that runs over a following map stays fail-closed ----------------------

def test_r6_gomap_value_over_next_map_fails_closed():
    """Known over-mask cost: ``next-pod`` between two Go maps is masked with the first value.
    Its span runs to the next ``map[name:``, and a cut at ``]]`` would leak a secret that
    holds ``]]``. Only the secret must be absent here."""
    text = "[map[name:API_TOKEN value:x]] next-pod [map[name:DB_PASSWORD value:" + V + "]]"
    for out in _plain(text):
        assert V not in out, out
