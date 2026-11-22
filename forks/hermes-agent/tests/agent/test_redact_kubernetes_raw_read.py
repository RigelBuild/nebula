"""RIG-11984: kubectl reads that print raw values the per-line Secret pass cannot see.

``kubectl exec <pod> -- <cmd>`` prints whatever ``<cmd>`` reads, and ``kubectl config view``
with ``--raw`` or ``--flatten`` prints the kubeconfig credentials unredacted. Both mask the
whole output. Keep rows: a plain ``config view`` and ``logs`` still show their content.
"""

import pytest

from agent.redact import redact_terminal_output

V = "zzqq" + "-fixture-leak-value-A11984"
K = "keep-visible-a11984"


@pytest.fixture(autouse=True)
def _ensure_redaction_enabled(monkeypatch):
    monkeypatch.delenv("HERMES_REDACT_SECRETS", raising=False)
    monkeypatch.setattr("agent.redact._REDACT_ENABLED", True)


def _terminal(text, command):
    return [redact_terminal_output(text, command, force=True), redact_terminal_output(text, command)]


# --- Bypass rows: the whole output is masked ---------------------------------------------------

# R1: exec runs a command after ``--``; its output is whatever the command reads.
R1_EXEC_COMMAND = [
    ("exec-cat-secret-file", "kubectl exec p -- cat /var/run/secrets/x"),
    ("exec-it-shell", "kubectl exec -it p -- sh -c 'cat x'"),
    ("exec-split-i-t-flags", "kubectl exec -i -t p -- cat x"),
]

# R2: config view with --raw or --flatten prints the kubeconfig credentials.
R2_CONFIG_VIEW_RAW = [
    ("config-view-raw", "kubectl config view --raw"),
    ("config-view-flatten", "kubectl config view --flatten"),
    ("config-view-raw-value", "kubectl config view --raw=true"),
    ("config-view-flatten-value", "kubectl config view --flatten=true"),
]


@pytest.mark.parametrize("command", [c for _, c in R1_EXEC_COMMAND], ids=[i for i, _ in R1_EXEC_COMMAND])
def test_r1_exec_command_masks_whole_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


@pytest.mark.parametrize("command", [c for _, c in R2_CONFIG_VIEW_RAW], ids=[i for i, _ in R2_CONFIG_VIEW_RAW])
def test_r2_config_view_raw_masks_whole_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


# --- Keep rows: a clean read shows its content --------------------------------------------------


def test_keep_config_view_without_flags():
    text = "current-context: " + K + "\n"
    for out in _terminal(text, "kubectl config view"):
        assert out != "***", out
        assert K in out, out


def test_keep_logs():
    text = "INFO started " + K + "\n"
    for out in _terminal(text, "kubectl logs p"):
        assert out != "***", out
        assert K in out, out
