"""RIG-11984 and RIG-11965: kubectl reads that print raw values the per-line Secret pass cannot see.

``kubectl exec``, ``attach``, ``debug``, ``run`` and ``cp`` print what a container or file holds.
``kubectl get --raw`` and ``create token`` return raw API objects or bearer tokens. ``config view``
with ``--raw`` or ``--flatten`` prints the kubeconfig credentials unredacted. All of these mask the
whole output. Keep rows: a plain ``config view``, ``logs`` and ``get pods`` still show their content.
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

# R3: debug and run start a container whose command prints what it reads, like exec.
R3_DEBUG_RUN = [
    ("debug-cat-secret-file", "kubectl debug p -it --image=busybox -- cat /var/run/secrets/x"),
    ("run-cat-secret-file", "kubectl run t --rm -i --image=busybox --restart=Never -- cat /var/run/secrets/x"),
]

# R4: cp to /dev/stdout prints the copied file.
R4_CP_STDOUT = [
    ("cp-to-dev-stdout", "kubectl cp p:/var/run/secrets/x /dev/stdout"),
]

# R5: get --raw prints a raw API path, so a Secret comes back as a whole object.
R5_GET_RAW = [
    ("get-raw-space", "kubectl get --raw /api/v1/namespaces/d/secrets/s"),
    ("get-raw-equals", "kubectl get --raw=/api/v1/namespaces/d/secrets/s"),
]

# R6: create token prints a bearer token for a service account.
R6_CREATE_TOKEN = [
    ("create-token", "kubectl create token sa-name"),
    ("create-token-duration", "kubectl create token sa-name -n ns --duration=10m"),
]

# R7: attach streams container output; exec with no ``--`` runs the words after the pod.
R7_ATTACH_EXEC_NO_DASH = [
    ("attach-it", "kubectl attach p -it"),
    ("exec-no-dash", "kubectl exec p cat /var/run/secrets/x"),
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


@pytest.mark.parametrize("command", [c for _, c in R3_DEBUG_RUN], ids=[i for i, _ in R3_DEBUG_RUN])
def test_r3_debug_run_masks_whole_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


@pytest.mark.parametrize("command", [c for _, c in R4_CP_STDOUT], ids=[i for i, _ in R4_CP_STDOUT])
def test_r4_cp_to_stdout_masks_whole_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


@pytest.mark.parametrize("command", [c for _, c in R5_GET_RAW], ids=[i for i, _ in R5_GET_RAW])
def test_r5_get_raw_masks_whole_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


@pytest.mark.parametrize("command", [c for _, c in R6_CREATE_TOKEN], ids=[i for i, _ in R6_CREATE_TOKEN])
def test_r6_create_token_masks_whole_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


@pytest.mark.parametrize("command", [c for _, c in R7_ATTACH_EXEC_NO_DASH], ids=[i for i, _ in R7_ATTACH_EXEC_NO_DASH])
def test_r7_attach_and_exec_without_dash_mask_whole_output(command):
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


def test_keep_get_pods_table():
    text = "NAME READY\nweb 1/1 Running\n"
    for out in _terminal(text, "kubectl get pods"):
        assert out != "***", out
        assert "web 1/1 Running" in out, out
