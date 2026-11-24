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

def _terminal_result(text, command, session_key):
    import json

    from tools.terminal_tool_result import finalize_foreground_result

    result = finalize_foreground_result(
        command=command, result={"output": text, "returncode": 0}, env=object(), env_type="local",
        effective_task_id=session_key, task_id=session_key, session_id=None, session_key=session_key,
        workdir=None, command_cwd="/tmp", approval_note=None,
    )
    return json.loads(result)["output"]


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
    ("cp-to-dash-stdout", "kubectl cp p:/var/run/secrets/x -"),
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


# R8: successful copies from a Secret mount stay tracked for later local file reads.
R8_COPY_SECRET_READBACK = [
    ("kubectl-cp", "kubectl cp p:/var/run/secrets/x ./copy", "cat ./copy"),
    ("oc-rsync", "oc rsync p:/var/run/secrets/x ./copy", "cat ./copy"),
]


@pytest.mark.parametrize(
    "copy_command,read_command",
    [(copy, read) for _, copy, read in R8_COPY_SECRET_READBACK],
    ids=[label for label, _, _ in R8_COPY_SECRET_READBACK],
)
def test_r8_secret_copy_readback_masks_output_and_audits(caplog, copy_command, read_command):
    session_key = f"secret-copy-{copy_command.split()[0]}"
    copied = _terminal_result("copy complete", copy_command, session_key)
    assert copied == "***", copied

    with caplog.at_level("INFO", logger="agent.redact"):
        read_back = _terminal_result(V, read_command, session_key)
    assert read_back == "***", read_back
    assert V not in caplog.text
    assert "secret copy" in caplog.text.lower()

    other_session = _terminal_result(V, read_command, session_key + "-other")
    assert V in other_session, other_session


def test_r8_secret_copy_detection_leaves_non_secret_source_readable():
    session_key = "non-secret-copy"
    _terminal_result("copy complete", "kubectl cp ./input p:/etc/config", session_key)
    read_back = _terminal_result(K, "cat ./copy", session_key)
    assert K in read_back, read_back


@pytest.mark.parametrize("reader", ["cat", "head", "tail", "less", "grep pattern"])
def test_r8_pod_copy_readers_and_nested_paths(reader):
    session_key = f"pod-copy-reader-{reader}"
    _terminal_result("done", "oc cp ns/pod:/arbitrary/mount ./directory", session_key)
    assert _terminal_result(V, f"{reader} /tmp/directory/item", session_key) == "***"


@pytest.mark.parametrize("command", [
    "kubectl cp -n ns -c ctr pod:/x ./y",
    "kubectl -n ns cp pod:/x ./y",
    "oc rsync --exclude=x pod:/dir ./y",
    "kubectl cp --container ctr pod:/x ./y",
    "oc rsync --exclude x --include y pod:/dir ./y",
    "kubectl cp --context ctx --kubeconfig cfg --retries 2 pod:/x ./y",
])
def test_r8_copy_options_before_operands(command):
    session_key = f"copy-options-{command}"
    _terminal_result("done", command, session_key)
    assert _terminal_result(V, "cat ./y", session_key) == "***"


def test_r8_failed_copy_attempt_is_tracked():
    from tools.terminal_tool_result import finalize_foreground_result

    key = "failed-pod-copy"
    finalize_foreground_result(command="kubectl cp p:/data ./failed; false", result={"output": "error", "returncode": 1},
                               env=object(), env_type="local", effective_task_id=key, task_id=key,
                               session_id=None, session_key=key, workdir=None, command_cwd="/tmp", approval_note=None)
    assert _terminal_result(V, "cat ./failed", key) == "***"


@pytest.mark.parametrize("copy,read", [
    ("sudo -u root -E kubectl cp p:/x ./y", "sudo -E cat ./y"),
    ("kubectl cp p:/x ./y", "base64 ./y"),
    ("kubectl cp p:var/data ./y", "cat ./y"),
    ("kubectl cp p:/x - > ./y", "cat ./y"),
    ("kubectl cp p:/x - >> ./y", "cat ./y"),
    ("kubectl cp p:/x - | tee -a ./y", "cat ./y"),
    ("kubectl cp p:/x ./sub/y", "cd sub && cat y"),
])
def test_r8_review_bypasses(copy, read):
    key = f"review-{copy}-{read}"
    _terminal_result("done", copy, key)
    assert _terminal_result(V, read, key) == "***"


def test_r8_session_cleanup_and_limit():
    from agent.redact import _POD_COPY_MAX_PER_SESSION, _POD_COPY_PATHS
    from tools.terminal_tool import _qualify_task_key, clear_session_cwd

    key = "review-cleanup"
    for i in range(_POD_COPY_MAX_PER_SESSION + 1):
        _terminal_result("done", f"kubectl cp p:/x ./file{i}", key)
    scope = _qualify_task_key(key)
    assert len(_POD_COPY_PATHS[scope]) == _POD_COPY_MAX_PER_SESSION + 1
    assert _terminal_result(V, "cat ./file0", key) == "***"
    clear_session_cwd(key)
    assert scope not in _POD_COPY_PATHS
    assert _terminal_result(V, "cat ./file256", key) == V


def test_r8_audit_distinguishes_pod_copy(caplog):
    key = "review-audit"
    _terminal_result("done", "kubectl cp p:/other ./y", key)
    with caplog.at_level("INFO", logger="agent.redact"):
        assert _terminal_result(V, "cat ./y", key) == "***"
    assert "pod copy read-back" in caplog.text
    assert "secret copy" not in caplog.text


@pytest.mark.parametrize("command", [
    "grep -r . .", "grep -r s /tmp", "cat *", "cat y*", "bash -c 'cat y'",
    "(cd sub && cat y)", "pushd sub && cat y", "cd\tsub && cat y",
    "cd sub && cat d/file", "cat <y", "dd if=y", "cat renamed",
])
def test_r8_local_value_masks_any_command(tmp_path, command, caplog):
    import json
    from types import SimpleNamespace
    from tools.terminal_tool import clear_session_cwd
    from tools.terminal_tool_result import finalize_foreground_result

    (tmp_path / "y").write_text(V)
    key = f"local-{command}"
    def run(cmd, text):
        return json.loads(finalize_foreground_result(
            command=cmd, result={"output": text, "returncode": 0}, env=SimpleNamespace(uses_host_filesystem=True),
            env_type="local", effective_task_id=key, task_id=key, session_id=None, session_key=key,
            workdir=None, command_cwd=str(tmp_path), approval_note=None))["output"]
    run("kubectl cp p:/var/run/secrets/x ./y", "done")
    with caplog.at_level("INFO", logger="agent.redact"):
        assert run(command, V) == "***"
    assert V not in caplog.text
    assert run("echo y", "y") == "y"
    assert run("rm y", "y") == "y"
    clear_session_cwd(key)
    assert run(command, V) == V


@pytest.mark.parametrize("copy", [
    "sudo -iu root kubectl cp p:/x ./y", "env K=v kubectl cp p:/x ./y",
    "command kubectl cp p:/x ./y", "time kubectl cp p:/x ./y", "doas kubectl cp p:/x ./y",
    "kubectl cp p:/x - &> ./y", "kubectl cp p:/x - >| ./y",
    "kubectl cp p:/x - | sudo tee ./y", "kubectl cp p:/x /dev/fd/1 > ./y",
])
def test_r8_local_copy_wrappers_and_streams(tmp_path, copy):
    import json
    from types import SimpleNamespace
    from tools.terminal_tool_result import finalize_foreground_result

    (tmp_path / "y").write_text(V)
    key = f"local-copy-{copy}"
    def run(cmd, text):
        return json.loads(finalize_foreground_result(
            command=cmd, result={"output": text, "returncode": 0}, env=SimpleNamespace(uses_host_filesystem=True),
            env_type="local", effective_task_id=key, task_id=key, session_id=None, session_key=key,
            workdir=None, command_cwd=str(tmp_path), approval_note=None))["output"]
    run(copy, "done")
    assert run("bash -c 'cat y'", V) == "***"


def test_r8_local_directory_values_and_limits(tmp_path):
    from agent.redact import _POD_COPY_VALUES, clear_pod_copies, record_pod_copy, redact_pod_copy_values

    root = tmp_path / "copied"
    root.mkdir()
    (root / "one").write_text("TOKEN=" + V + "\n" + "x" * 300)
    (root / "binary").write_bytes(b"\x00binary-secret-value")
    (root / "oversize").write_text("z" * 65537)
    key = "local-directory-values"
    record_pod_copy("oc rsync p:/data ./copied", key, str(tmp_path), local=True)
    assert redact_pod_copy_values(V, key) == "***"
    assert redact_pod_copy_values("binary-secret-value", key) == "binary-secret-value"
    assert all(len(value) <= 65536 for value in _POD_COPY_VALUES[key])
    clear_pod_copies(key)
    assert key not in _POD_COPY_VALUES


def test_r8_local_stdout_value(tmp_path):
    from agent.redact import clear_pod_copies, record_pod_copy, redact_pod_copy_values

    key = "local-stdout-value"
    record_pod_copy("kubectl cp p:/data /dev/stdout", key, str(tmp_path), local=True, output=V)
    assert redact_pod_copy_values(V, key) == "***"
    clear_pod_copies(key)


def test_r8_value_formats_and_overlap(tmp_path):
    import base64
    from agent.redact import clear_pod_copies, record_pod_copy, redact_pod_copy_values

    key = "formats-copy"
    (tmp_path / "y").write_text('{"password": "abcdefgh12SECRETTAIL99"}')
    record_pod_copy("kubectl cp p:/x y", key, str(tmp_path), local=True)
    value = "abcdefgh12SECRETTAIL99"
    for text in (value, base64.b64encode(value.encode()).decode(), value.encode().hex()):
        assert redact_pod_copy_values(text, key) == "***"
    clear_pod_copies(key)


@pytest.mark.parametrize("kind", ["many-files", "large", "binary", "non-utf8", "missing"])
def test_r8_bounds_fail_closed(tmp_path, kind, caplog):
    from agent.redact import clear_pod_copies, pod_copy_read, record_pod_copy, redact_pod_copy_values

    path = tmp_path / "y"
    if kind == "many-files":
        path.mkdir()
        for n in range(65):
            (path / str(n)).write_text("secret-value-123")
    elif kind == "large":
        path.write_bytes(b"x" * 65537)
    elif kind == "binary":
        path.write_bytes(b"\x00secret-value")
    elif kind == "non-utf8":
        path.write_bytes(b"\xffsecret-value")
    key = "bounds-" + kind
    record_pod_copy("kubectl cp p:/x y", key, str(tmp_path), local=True)
    assert redact_pod_copy_values("hello", key) == "hello"
    assert pod_copy_read("cat y", key, str(tmp_path)) is not None
    assert V not in caplog.text
    clear_pod_copies(key)


def test_r8_copy_destination_symlink_is_not_read(tmp_path):
    from agent.redact import _POD_COPY_VALUES, clear_pod_copies, record_pod_copy

    target = tmp_path / "outside"
    target.write_text(V)
    (tmp_path / "link").symlink_to(target)
    key = "link-copy"
    record_pod_copy("kubectl cp p:/x link", key, str(tmp_path), local=True)
    assert V not in _POD_COPY_VALUES.get(key, {})
    clear_pod_copies(key)



def test_r8_plain_words_and_multiline_tokens(tmp_path):
    from agent.redact import clear_pod_copies, record_pod_copy, redact_pod_copy_values

    (tmp_path / "cfg").write_text("kind: ConfigMap\nnamespace: production\nname: application\n# tokens\n" + V + "\n")
    key = "plain-words"
    record_pod_copy("kubectl cp p:/x cfg", key, str(tmp_path), local=True)
    assert redact_pod_copy_values("kind: ConfigMap namespace: production Pod application-7f9", key) == "kind: ConfigMap namespace: production Pod application-7f9"
    assert redact_pod_copy_values(V, key) == "***"
    clear_pod_copies(key)


def test_r8_deployment_keys_remain_visible(tmp_path):
    from agent.redact import clear_pod_copies, record_pod_copy, redact_pod_copy_values

    (tmp_path / "deployment").write_text("metadata:\n  template:\n    containers:\n      - name: app\n")
    key = "deployment-keys"
    record_pod_copy("kubectl cp p:/x deployment", key, str(tmp_path), local=True)
    text = "metadata:\n  template:\n    containers:"
    assert redact_pod_copy_values(text, key) == text
    clear_pod_copies(key)


@pytest.mark.parametrize("contents,value", [
    ("tokens:\n  - secret-value-123\n", "secret-value-123"),
    ("tokens:\nc2VjcmV0LXZhbHVlLTEyMw==\n", "c2VjcmV0LXZhbHVlLTEyMw=="),
    ("tokens:\n#secret-value-123\n", "secret-value-123"),
], ids=["yaml-list", "base64-padding", "comment"])
def test_r8_indirect_read_masks_list_and_padded_tokens(tmp_path, contents, value):
    import json
    from types import SimpleNamespace
    from tools.terminal_tool import clear_session_cwd
    from tools.terminal_tool_result import finalize_foreground_result

    (tmp_path / "tok").write_text(contents)
    key = f"indirect-{contents}"
    def run(command, output):
        result = finalize_foreground_result(
            command=command, result={"output": output, "returncode": 0},
            env=SimpleNamespace(uses_host_filesystem=True), env_type="local",
            effective_task_id=key, task_id=key, session_id=None, session_key=key,
            workdir=None, command_cwd=str(tmp_path), approval_note=None)
        return json.loads(result)["output"]
    run("kubectl cp pod:/tokens ./tok", "copied")
    assert run("cat $(ls)", value) == "***"
    clear_session_cwd(key)


def test_r8_binary_copy_does_not_black_out_session(tmp_path):
    from agent.redact import clear_pod_copies, pod_copy_read, record_pod_copy, redact_pod_copy_values

    (tmp_path / "images").mkdir()
    (tmp_path / "images" / "x.png").write_bytes(b"\x00PNG")
    key = "binary-dir"
    record_pod_copy("oc rsync p:/x images", key, str(tmp_path), local=True)
    assert redact_pod_copy_values("hello", key) == "hello"
    assert pod_copy_read("cat images/x.png", key, str(tmp_path)) is not None
    clear_pod_copies(key)

@pytest.mark.parametrize("command", [
    "cd sub && kubectl cp p:/x y",
    "(cd sub && kubectl cp p:/x y)",
    "timeout 30 kubectl cp p:/x sub/y",
    "stdbuf -o0 kubectl cp p:/x sub/y",
    "env -u FOO kubectl cp p:/x sub/y",
    "bash -c 'kubectl cp p:/x sub/y'",
    "kubectl cp p:/x ~/sub/y",
    "kubectl cp p:/x $HOME/sub/y",
])
def test_r8_local_copy_resolution(tmp_path, monkeypatch, command):
    from agent.redact import clear_pod_copies, record_pod_copy, redact_pod_copy_values

    monkeypatch.setenv("HOME", str(tmp_path))
    (tmp_path / "sub").mkdir()
    (tmp_path / "sub" / "y").write_text(V)
    key = "resolve-" + command
    record_pod_copy(command, key, str(tmp_path), local=True)
    assert redact_pod_copy_values(V, key) == "***"
    clear_pod_copies(key)


def test_r8_dash_destination(tmp_path):
    from agent.redact import clear_pod_copies, record_pod_copy, redact_pod_copy_values

    (tmp_path / "-").write_text(V)
    key = "dash-file"
    record_pod_copy("kubectl cp p:/x -", key, str(tmp_path), local=True)
    assert redact_pod_copy_values(V, key) == "***"
    clear_pod_copies(key)


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
