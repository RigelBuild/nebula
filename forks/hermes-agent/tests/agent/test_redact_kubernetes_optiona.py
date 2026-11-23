"""Option A for RIG-11965: a kubectl/oc read masks the whole output unless the command is
clean, every output-format flag is structured, and it is not ``set env --list``.

Bypass rows: shapes that the per-line Secret pass cannot see (wrapper, substitution,
pipe-to-filter, mixed ``-o``, Windows path, manifest input, short-flag cluster,
``set env --list``, ``oc``).
Keep rows: clean structured or table reads that must still show their content.
"""

import pytest

from agent.redact import redact_terminal_output

V = "zzqq" + "-fixture-leak-value-A11965"
K = "keep-visible-a11965"


@pytest.fixture(autouse=True)
def _ensure_redaction_enabled(monkeypatch):
    monkeypatch.delenv("HERMES_REDACT_SECRETS", raising=False)
    monkeypatch.setattr("agent.redact._REDACT_ENABLED", True)


def _terminal(text, command):
    return [redact_terminal_output(text, command, force=True), redact_terminal_output(text, command)]


# --- Bypass rows: the whole output is masked ---------------------------------------------------

# F1: a shell wrapper or runner hides the kubectl word from the first-token check.
F1_WRAPPER = [
    ("bash-lc", "bash -lc \"kubectl get secret s -o jsonpath='{.data.k}'\""),
    ("sh-ec", "sh -ec \"kubectl get secret s -o jsonpath='{.data.k}'\""),
    ("bash-login-c", "bash --login -c \"kubectl get secret s -o jsonpath='{.data.k}'\""),
    ("watch", "watch 'kubectl get secret s -o jsonpath={.data.k}'"),
    ("ssh", "ssh host \"kubectl get secret s -o jsonpath='{.data.k}'\""),
    ("eval", "eval \"kubectl get secret s -o jsonpath='{.data.k}'\""),
    ("sudo-bash-c", "sudo -u k bash -c \"kubectl get secret s -o jsonpath='{.data.password}'\""),
]

# F1b: a command substitution or subshell wraps the kubectl read.
F1B_SUBSTITUTION = [
    ("dollar-subst", "echo $(kubectl get secret s -o jsonpath='{.data.k}' | base64 -d)"),
    ("backtick-subst", "echo `kubectl get secret s -o jsonpath='{.data.k}'` | base64 -d"),
    ("subshell", "(kubectl get secret s -o jsonpath='{.data.k}')"),
]

# F2: a pipe to a filter after a structured read still exposes the value to the filter.
F2_PIPE = [
    ("json-to-jq", "kubectl get secret s -o json | jq -r .data.password"),
    ("yaml-to-grep", "kubectl get secret s -o yaml | grep password"),
]

# F3: a second -o flag can be unstructured while the first one is structured.
F3_MIXED_OUTPUT = [
    ("yaml-then-jsonpath", "kubectl get secret s -o yaml -o jsonpath='{.data.password}'"),
    ("global-yaml-then-jsonpath", "kubectl -o yaml get secret s -o jsonpath='{.data.password}'"),
]

# F4: a Windows binary path or a quoted path hides the kubectl basename.
F4_WINDOWS_PATH = [
    ("unquoted-exe", "C:\\tools\\kubectl.exe get secret s -o jsonpath='{.data.k}'"),
    ("quoted-exe", "'C:\\tools\\kubectl.exe' get secret s -o jsonpath='{.data.k}'"),
]

# F5: a manifest or kustomize input has no Secret resource token to match on.
F5_MANIFEST = [
    ("filename", "kubectl get -f manifests/ -o jsonpath='{.items[*].data}'"),
    ("kustomize", "kubectl get -k overlays/prod -o jsonpath='{.items[*].data}'"),
]

# F8: the oc binary is a kubectl name too.
F8_OC = [
    ("oc-secret-jsonpath", "oc get secret s -o jsonpath='{.data.k}'"),
]

# F9: a short-flag cluster hides an ``o`` output flag (``-Ao``, ``-wo``, ``-oA``, ``-Aojson``).
F9_SHORT_CLUSTER = [
    ("Ao-jsonpath", "kubectl get secret s -Ao jsonpath='{.data.k}'"),
    ("wo-jsonpath", "kubectl get secret s -wo jsonpath='{.data.k}'"),
    ("oA-jsonpath", "kubectl get secret s -oA jsonpath='{.data.k}'"),
    ("Aojson", "kubectl get secret s -Aojson"),
    ("Aojsonpath-brace", "kubectl get secret s -Aojsonpath={.data.k}"),
    ("Ao-eq-jsonpath", "kubectl get secret s -Ao=jsonpath={.data.k}"),
    ("wo-quoted", "kubectl get secret s -wo'jsonpath={.data.k}'"),
    ("Aogo-template", "kubectl get secret s -Aogo-template={{.data.k}}"),
]

# F10: bash brace expansion turns ``{-o,jsonpath=...}`` into ``-o jsonpath=...``; shlex keeps it one token.
F10_BRACE = [
    ("brace-o-jsonpath", "kubectl get secret x {-o,jsonpath={.data.password}}"),
    ("brace-oyaml-ojsonpath", "kubectl get secret x {-oyaml,-ojsonpath={.data}}"),
    ("glob-bracket-ojsonpath", "kubectl get secret x -o yaml [-]ojsonpath=.data.password"),
    ("glob-star-ojsonpath", "kubectl get secret x -o yaml *ojsonpath=.data.password"),
    ("split-empty-quotes", "kube''ctl get pods -o yaml"),
    ("split-double-quotes", "\"kube\"ctl config view --raw"),
    ("split-backslash", "kube\\ctl exec web -- env"),
    ("split-double-empty", "kube\"\"ctl get secret s -o yaml"),
]


# F11: quoting or escaping the kubectl name splits it from the mention check.
F11_SPLIT_NAME = [row for row in F10_BRACE if row[0].startswith("split-")]
PLAIN = "plainvalue-hunter2"
# F12: the shell builds the name from ``$``, a backtick, braces or a line continuation.
F12_BUILT_NAME = [
    ("dollar-empty-quotes", "kube$''ctl get pods -o yaml"),
    ("backtick-name", "kube``ctl get pods -o yaml"),
    ("dollar-subst", "kube$()ctl get pods -o yaml"),
    ("dollar-brace", "kube${x}ctl get pods -o yaml"),
    ("backslash-newline", "kube\\\nctl get pods -o yaml"),
    ("newline-before-name", "echo x\nkubectl get secret s -o jsonpath={.data}"),
    ("crlf-before-name", "true\r\nkubectl get secret s -o jsonpath={.data}"),
    ("ifs-glued-name", "kubectl${IFS}get secret s -o jsonpath={.data}"),
    ("ifs-glued-k", "k${IFS}get secret s -o jsonpath={.data}"),
    ("paren-before-name", "echo)kubectl get secret s -o jsonpath={.data}"),
    ("brace-before-name", "echo}kubectl get secret s -o jsonpath={.data}"),
    ("subst-glued-name", "kubectl$(printf ' ')get secret s -o jsonpath={.data}"),
]


@pytest.mark.parametrize("command", [c for _, c in F12_BUILT_NAME], ids=[i for i, _ in F12_BUILT_NAME])
def test_f12_built_command_name_masks_whole_output(command):
    text = PLAIN + "\n"
    for out in _terminal(text, command):
        assert PLAIN not in out, out
        assert out == "***", out


# Nemo condition (RIG-11965): describe on deployments masks env values; logs still goes
# through the value redactor.
def test_describe_deploy_env_value_masked():
    text = "Containers:\n  app:\n    Environment:\n      DB_PASSWORD:  " + V + "\n      LOG_LEVEL:  info\n"
    for out in _terminal(text, "kubectl describe deploy web"):
        assert V not in out, out
        assert "LOG_LEVEL:  info" in out, out


def test_logs_still_runs_value_redactor():
    token = "ghp_" + "a" * 36
    text = "INFO fetched with " + token + "\n"
    for out in _terminal(text, "kubectl logs web -c app"):
        assert token not in out, out


def test_keep_plain_kubectl_name_stays_unmasked():
    text = "web 1/1 Running " + K + "\n"
    for out in _terminal(text, "kubectl get pods -o yaml"):
        assert out != "***", out
        assert K in out, out


@pytest.mark.parametrize("command", [c for _, c in F11_SPLIT_NAME], ids=[i for i, _ in F11_SPLIT_NAME])
def test_f11_split_kubectl_name_masks_whole_output(command):
    text = PLAIN + "\n"
    for out in _terminal(text, command):
        assert PLAIN not in out, out
        assert out == "***", out

@pytest.mark.parametrize("command", [c for n, c in F10_BRACE if not n.startswith("split-")], ids=[n for n, _ in F10_BRACE if not n.startswith("split-")])
def test_f10_brace_expansion_masks_whole_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


@pytest.mark.parametrize("command", [c for _, c in F1_WRAPPER], ids=[i for i, _ in F1_WRAPPER])
def test_f1_wrapper_masks_whole_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


@pytest.mark.parametrize("command", [c for _, c in F1B_SUBSTITUTION], ids=[i for i, _ in F1B_SUBSTITUTION])
def test_f1b_substitution_masks_whole_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


@pytest.mark.parametrize("command, text", [
    ("kubectl get secret s -o json | jq -r .data.password", V + "\n"),
    ("kubectl get secret s -o yaml | grep password", "  password: " + V + "\n"),
])
def test_f2_pipe_to_filter_masks_whole_output(command, text):
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


@pytest.mark.parametrize("command", [c for _, c in F3_MIXED_OUTPUT], ids=[i for i, _ in F3_MIXED_OUTPUT])
def test_f3_mixed_output_flags_mask_whole_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


@pytest.mark.parametrize("command", [c for _, c in F4_WINDOWS_PATH], ids=[i for i, _ in F4_WINDOWS_PATH])
def test_f4_windows_path_masks_whole_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


@pytest.mark.parametrize("command", [c for _, c in F5_MANIFEST], ids=[i for i, _ in F5_MANIFEST])
def test_f5_manifest_input_masks_whole_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


@pytest.mark.parametrize("command", ["kubectl set env deploy/w --list --resolve"])
def test_f6_set_env_list_masks_whole_output(command):
    text = "TLS_KEY=-----BEGIN-----\n" + V + "\n-----END-----\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


@pytest.mark.parametrize("command", [c for _, c in F8_OC], ids=[i for i, _ in F8_OC])
def test_f8_oc_masks_whole_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


@pytest.mark.parametrize("command", [c for _, c in F9_SHORT_CLUSTER], ids=[i for i, _ in F9_SHORT_CLUSTER])
def test_f9_short_flag_cluster_masks_whole_output(command):
    text = V + "\n"
    for out in _terminal(text, command):
        assert V not in out, out
        assert out == "***", out


# F7 (low, accepted over-mask by design): ``k`` as a standalone word in a non-kubectl command
# masks the output. Baseline already masks it; Option A keeps it that way.
def test_f7_standalone_k_word_in_grep_masks_accepted_over_mask():
    text = V + "\n"
    for out in _terminal(text, "grep k secrets.txt -o"):
        assert V not in out, out
        assert out == "***", out


# --- Keep rows: a clean read shows its content --------------------------------------------------


def test_keep_get_pods_all_namespaces_short_flag():
    text = "NAME READY STATUS\napi-7f9 1/1 Running " + K + "\n"
    for out in _terminal(text, "kubectl get pods -A"):
        assert out != "***", out
        assert K in out, out


def test_keep_get_pods_table():
    text = "NAME READY STATUS\napi-7f9 1/1 Running " + K + "\n"
    for out in _terminal(text, "kubectl get pods"):
        assert out != "***", out
        assert K in out, out


def test_keep_get_pods_wide_table():
    text = ("NAME READY STATUS RESTARTS AGE IP NODE\n"
            "api-7f9 1/1 Running 0 2d 10.0.0.1 " + K + "\n")
    for out in _terminal(text, "kubectl get pods -o wide"):
        assert out != "***", out
        assert K in out, out


def test_keep_describe_configmap():
    text = "Name:  cm\nData\n====\nkey:\n----\n" + K + "\n"
    for out in _terminal(text, "kubectl describe configmap cm"):
        assert out != "***", out
        assert K in out, out


def test_keep_get_deploy_yaml():
    text = (
        "apiVersion: apps/v1\n"
        "kind: Deployment\n"
        "metadata:\n"
        "  name: web\n"
        "spec:\n"
        "  replicas: 2\n"
        "  template:\n"
        "    spec:\n"
        "      containers:\n"
        "      - name: app\n"
        "        image: " + K + "-image\n"
        "        env:\n"
        "        - name: DB_PASSWORD\n"
        "          value: " + V + "\n"
    )
    for out in _terminal(text, "kubectl get deploy web -o yaml"):
        assert V not in out, out
        assert K in out, out
        assert "replicas: 2" in out, out


def test_keep_logs():
    text = "INFO started " + K + "\n"
    for out in _terminal(text, "kubectl logs pod -c app"):
        assert out != "***", out
        assert K in out, out
