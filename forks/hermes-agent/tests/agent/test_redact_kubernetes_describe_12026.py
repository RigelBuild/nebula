"""RIG-12026: ``kubectl describe`` leaks the values under ``Command:``, ``Args:`` and a CRD's
``Spec:``. Rows go through ``redact_terminal_output`` (the entry point tool output uses), with
``force`` on and off, so a row passes whichever layer masks it.

Leak rows: a literal value in Command, Args, or a CRD Spec must not reach the output.
Keep rows: Name, Status, Events, and node or namespace describes stay readable.
"""

import pytest

from agent.redact import redact_terminal_output

V = "zzqq" + "-fixture-leak-value-A12026"
K = "keep-visible-a12026"


@pytest.fixture(autouse=True)
def _ensure_redaction_enabled(monkeypatch):
    monkeypatch.delenv("HERMES_REDACT_SECRETS", raising=False)
    monkeypatch.setattr("agent.redact._REDACT_ENABLED", True)


def _terminal(text, command):
    return [redact_terminal_output(text, command, force=True), redact_terminal_output(text, command)]


POD_ARGS = (
    "Name:             web\n"
    "Containers:\n"
    "  app:\n"
    "    Image:      nginx\n"
    "    Args:\n"
    "      --db-pass=" + V + "\n"
    "    State:          Running\n"
)

POD_COMMAND = (
    "Name:             web\n"
    "Containers:\n"
    "  app:\n"
    "    Command:\n"
    "      /bin/sh\n"
    "      -c\n"
    "      run.sh " + V + "\n"
    "    State:          Running\n"
)

DEPLOY_COMMAND_INLINE = (
    "Name:                   web\n"
    "Pod Template:\n"
    "  Containers:\n"
    "   app:\n"
    "    Command:  run.sh --token=" + V + "\n"
    "    Replicas:  2\n"
)

CRD_SPEC = (
    "Name:         foo\n"
    "Namespace:    flux-system\n"
    "Kind:         HelmRelease\n"
    "Spec:\n"
    "  Chart:\n"
    "    Spec:\n"
    "      Chart:  podinfo\n"
    "  Values:\n"
    "    adminPassword:  " + V + "\n"
    "Status:\n"
    "  Conditions:\n"
    "    Type:    Ready\n"
    "    Status:  True\n"
)

POD_ENV_SECRET = (
    "Name:             web\n"
    "Containers:\n"
    "  app:\n"
    "    Environment:\n"
    "      DB_PASSWORD:  " + V + "\n"
    "      LOG_LEVEL:  info\n"
)


# --- Leak rows: the value must not reach the output -------------------------------------------

@pytest.mark.parametrize("command", ["kubectl describe pod web", "kubectl describe pod web -n default"])
def test_describe_pod_args_value_masked(command):
    for out in _terminal(POD_ARGS, command):
        assert V not in out, out


def test_describe_pod_command_value_masked():
    for out in _terminal(POD_COMMAND, "kubectl describe pod web"):
        assert V not in out, out
        assert "State:          Running" in out, out


def test_describe_deploy_command_inline_value_masked():
    for out in _terminal(DEPLOY_COMMAND_INLINE, "kubectl describe deploy web"):
        assert V not in out, out
        assert "Replicas:  2" in out, out


def test_describe_crd_spec_value_masked():
    for out in _terminal(CRD_SPEC, "kubectl describe crd helmreleases.helm.toolkit.fluxcd.io foo"):
        assert V not in out, out


def test_describe_pod_env_secret_value_masked():
    # Already masked before RIG-12026; pinned so the workload env path cannot regress.
    for out in _terminal(POD_ENV_SECRET, "kubectl describe pod web"):
        assert V not in out, out
        assert "LOG_LEVEL:  info" in out, out


# --- Keep rows: harmless describe fields stay readable -----------------------------------------

def test_keep_crd_name_and_status_readable():
    for out in _terminal(CRD_SPEC, "kubectl describe crd helmreleases.helm.toolkit.fluxcd.io foo"):
        assert "Name:         foo" in out, out
        assert "Namespace:    flux-system" in out, out
        assert "Type:    Ready" in out, out


def test_keep_pod_events_readable():
    text = (
        "Name:             web\n"
        "Containers:\n"
        "  app:\n"
        "    State:          Running\n"
        "Events:\n"
        "  Type    Reason   Age   From     Message\n"
        "  Normal  Pulled   2m    kubelet  Container image pulled " + K + "\n"
    )
    for out in _terminal(text, "kubectl describe pod web"):
        assert K in out, out
        assert "Pulled" in out, out


def test_keep_node_describe_unmasked():
    text = (
        "Name:               node-1\n"
        "Roles:              worker\n"
        "Labels:             zone=" + K + "\n"
        "Conditions:\n"
        "  Type    Status\n"
        "  Ready   True\n"
    )
    for out in _terminal(text, "kubectl describe node node-1"):
        assert out != "***", out
        assert K in out, out


def test_keep_namespace_describe_unmasked():
    text = "Name:         team-" + K + "\nLabels:       env=dev\nStatus:       Active\n"
    for out in _terminal(text, "kubectl describe namespace team"):
        assert out != "***", out
        assert K in out, out
