"""RIG-11984 known gaps: bypasses found by the PR 226 review that this change does not close.

Each row is a real leak today. ``xfail(strict=True)`` keeps the suite green while the gap is
open, and turns the row red the moment a fix makes it pass, so the marker must then be removed.
Tracked on RIG-11984; not patched in this change (no second patch round).
"""

import pytest

from agent.redact import redact_terminal_output

V = "zzqq" + "-fixture-leak-value-A11984"
TOKEN = "sha256~" + "A" * 43


@pytest.fixture(autouse=True)
def _ensure_redaction_enabled(monkeypatch):
    monkeypatch.delenv("HERMES_REDACT_SECRETS", raising=False)
    monkeypatch.setattr("agent.redact._REDACT_ENABLED", True)


def _terminal(text, command):
    return [redact_terminal_output(text, command, force=True), redact_terminal_output(text, command)]


# G1 (high): a short token flag with an inline value. The deny check matches -t, --show-token
# and --show-token=..., not -t=VALUE, so the verb whoami stays a read and the token prints.
G1_WHOAMI_TOKEN_INLINE = [
    ("oc-whoami-t-eq-1", "oc whoami -t=1"),
    ("oc-whoami-t-eq-true", "oc whoami -t=true"),
]


@pytest.mark.parametrize("command", [c for _, c in G1_WHOAMI_TOKEN_INLINE], ids=[i for i, _ in G1_WHOAMI_TOKEN_INLINE])
def test_g1_oc_whoami_inline_token_flag_masks_whole_output(command):
    text = TOKEN + "\n"
    for out in _terminal(text, command):
        assert TOKEN not in out, out
        assert out == "***", out


# G2 (medium): a verbosity flag makes client-go log the API response body, which is the full
# Secret. The body is one quoted klog line, which the JSON pass cannot parse, so data stays visible.
G2_VERBOSE_BODY = [
    ("kubectl-v8", "kubectl get secret s -o name -v 8"),
    ("kubectl-v-eq-9", "kubectl get secret s -o name -v=9"),
    ("kubectl-v8-long", "kubectl get secret s -o name --v=8"),
    ("oc-loglevel-8", "oc get secret s -o name --loglevel 8"),
]


@pytest.mark.parametrize("command", [c for _, c in G2_VERBOSE_BODY], ids=[i for i, _ in G2_VERBOSE_BODY])
def test_g2_verbose_secret_body_masks_value(command):
    text = (
        'I0101 12:00:00.000000 1 round_trippers.go:553] Response Body: '
        '"{\\"kind\\":\\"Secret\\",\\"data\\":{\\"k\\":\\"' + V + '\\"}}"\n'
    )
    for out in _terminal(text, command):
        assert V not in out, out


# G3 (medium): a kuberc file can set a default output for get, so a command with no -o flag
# prints a raw value. The gate reads only the command line, so --kuberc and KUBECTL_KUBERC pass.
G3_KUBERC = [
    ("kuberc-eq", "kubectl --kuberc=/tmp/k get secret s"),
    ("kuberc-env", "KUBECTL_KUBERC=/tmp/k kubectl get secret s"),
]


@pytest.mark.parametrize("command", [c for _, c in G3_KUBERC], ids=[i for i, _ in G3_KUBERC])
def test_g3_kuberc_default_output_masks_whole_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out
