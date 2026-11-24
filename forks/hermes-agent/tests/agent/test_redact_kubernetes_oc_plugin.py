"""RIG-11984: oc-only raw reads and kubectl plugins print secret values the per-line Secret pass
cannot see. ``oc rsh``, ``oc extract``, ``oc whoami -t``, ``oc sa get-token`` and the ``oc`` forms
of exec, debug, cp, create token, get --raw and config view mask the whole output. A kubectl
plugin (a ``kubectl-<name>`` binary, or ``kubectl <name>``) runs code the masker cannot read, so
its whole output is masked too. Keep rows: built-in reads with the same shape still show content.
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


# --- oc raw reads: the whole output is masked ---------------------------------------------------

# O1: oc-only subcommands that stream a container or a Secret's files.
O1_OC_STREAM = [
    ("oc-rsh-cat", "oc rsh p cat /var/run/secrets/x"),
    ("oc-extract-stdout", "oc extract secret/s --to=-"),
    ("oc-extract-to-dir", "oc extract secret/s -n ns --to=/tmp/x"),
]

# O2: oc whoami prints the session token with -t or --show-token.
O2_OC_WHOAMI_TOKEN = [
    ("oc-whoami-t", "oc whoami -t"),
    ("oc-whoami-show-token", "oc whoami --show-token"),
    ("oc-whoami-show-token-value", "oc whoami --show-token=true"),
]

# O3: oc sa get-token prints a service account bearer token.
O3_OC_SA_GET_TOKEN = [
    ("oc-sa-get-token", "oc sa get-token sa-name"),
    ("oc-sa-get-token-ns", "oc sa get-token sa-name -n ns"),
]

# O4: the oc forms of the raw reads kubectl already masks.
O4_OC_SHARED_RAW = [
    ("oc-exec-cat", "oc exec p -- cat /var/run/secrets/x"),
    ("oc-debug-cat", "oc debug p -- cat x"),
    ("oc-cp-stdout", "oc cp p:/x /dev/stdout"),
    ("oc-create-token", "oc create token sa-name"),
    ("oc-get-raw", "oc get --raw /api/v1/namespaces/d/secrets/s"),
    ("oc-config-view-raw", "oc config view --raw"),
]

# P1: a kubectl plugin binary, named directly or run as ``kubectl <name>``. Its output is not a
# structured kubectl read, so it is masked whole, even with ``-o yaml``.
P1_PLUGIN = [
    ("plugin-direct", "kubectl-view-secret s -n ns"),
    ("plugin-direct-exe", "kubectl-view-secret.exe s"),
    ("plugin-subcommand", "kubectl view-secret s -n ns"),
    ("plugin-subcommand-o-yaml", "kubectl view-secret s -o yaml"),
    ("plugin-after-global-flag", "kubectl -n prod view-secret s"),
    ("plugin-k-alias", "k view-secret s"),
    ("plugin-oc", "oc view-secret s"),
]


@pytest.mark.parametrize("command", [c for _, c in O1_OC_STREAM], ids=[i for i, _ in O1_OC_STREAM])
def test_o1_oc_stream_masks_whole_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


@pytest.mark.parametrize("command", [c for _, c in O2_OC_WHOAMI_TOKEN], ids=[i for i, _ in O2_OC_WHOAMI_TOKEN])
def test_o2_oc_whoami_token_masks_whole_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


@pytest.mark.parametrize("command", [c for _, c in O3_OC_SA_GET_TOKEN], ids=[i for i, _ in O3_OC_SA_GET_TOKEN])
def test_o3_oc_sa_get_token_masks_whole_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


@pytest.mark.parametrize("command", [c for _, c in O4_OC_SHARED_RAW], ids=[i for i, _ in O4_OC_SHARED_RAW])
def test_o4_oc_shared_raw_reads_mask_whole_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


@pytest.mark.parametrize("command", [c for _, c in P1_PLUGIN], ids=[i for i, _ in P1_PLUGIN])
def test_p1_kubectl_plugin_masks_whole_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


# --- Keep rows: a built-in read with no raw shape still shows its content -----------------------


def test_keep_oc_whoami_without_token_flag():
    text = "alice\n"
    for out in _terminal(text, "oc whoami"):
        assert out != "***", out
        assert "alice" in out, out


def test_keep_kubectl_global_namespace_flag_before_get():
    text = "NAME READY\nweb 1/1 Running\n"
    for out in _terminal(text, "kubectl -n prod get pods"):
        assert out != "***", out
        assert "web 1/1 Running" in out, out


def test_keep_kubectl_version():
    text = "Client Version: " + K + "\n"
    for out in _terminal(text, "kubectl version"):
        assert out != "***", out
        assert K in out, out
