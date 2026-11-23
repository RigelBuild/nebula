"""Local macOS terminal sandboxing hides only the Hermes credential files."""

from unittest.mock import patch

import pytest

from tools.environments import local


def test_profile_denies_each_secret_file_but_not_logs(tmp_path, monkeypatch):
    home = tmp_path / ".hermes"
    monkeypatch.setenv("HERMES_HOME", str(home))

    profile = local._local_macos_secret_profile()

    for name in (".env", "auth.json", ".rr-token"):
        assert f'(deny file-read* file-write* file-link (literal "{(home / name).resolve()}"))' in profile
    assert "(allow default)" in profile
    assert str((home / "logs").resolve()) not in profile


def test_profile_denies_symlink_and_resolved_paths(tmp_path, monkeypatch):
    real_home = tmp_path / "real"
    real_home.mkdir()
    home = tmp_path / "link"
    home.symlink_to(real_home, target_is_directory=True)
    monkeypatch.setenv("HERMES_HOME", str(home))

    profile = local._local_macos_secret_profile()

    for name in (".env", "auth.json", ".rr-token"):
        for path in (home / name, real_home / name):
            assert f'(deny file-read* file-write* file-link (literal "{path}"))' in profile
    assert str(home / "logs") not in profile
    assert str(real_home / "logs") not in profile


def test_wraps_argv_on_darwin_when_sandbox_exec_exists(monkeypatch):
    monkeypatch.setattr(local.sys, "platform", "darwin")
    original = ["/bin/bash", "-c", "echo hi"]
    monkeypatch.setattr(local.os.path, "exists", lambda path: path == "/usr/bin/sandbox-exec")
    monkeypatch.setattr(local, "_local_macos_secret_profile", lambda: "profile")
    with patch("hermes_cli.config_effective.load_user_config_effective", return_value={"terminal": {}}):
        wrapped = local._sandbox_local_macos_argv(original)

    assert wrapped == ["/usr/bin/sandbox-exec", "-p", "profile", "/usr/bin/env", "HERMES_SECRET_SANDBOX=1", *original]


def test_leaves_argv_unchanged_on_other_platform(monkeypatch):
    monkeypatch.setattr(local.sys, "platform", "linux")
    original = ["/bin/bash", "-c", "echo hi"]

    assert local._sandbox_local_macos_argv(original) is original


def test_leaves_argv_unchanged_when_sandbox_exec_is_missing(monkeypatch, caplog):
    monkeypatch.setattr(local.sys, "platform", "darwin")
    monkeypatch.setattr(local, "_sandbox_missing_warned", False)
    original = ["/bin/bash", "-c", "echo hi"]
    monkeypatch.setattr(local.os.path, "exists", lambda path: False)
    with patch("hermes_cli.config_effective.load_user_config_effective", return_value={"terminal": {}}):
        wrapped = local._sandbox_local_macos_argv(original)
        local._sandbox_local_macos_argv(original)

    assert wrapped is original
    assert "sandbox-exec is unavailable" in caplog.text
    assert caplog.text.count("sandbox-exec is unavailable") == 1


def test_leaves_argv_unchanged_when_config_disables_sandbox(monkeypatch):
    monkeypatch.setattr(local.sys, "platform", "darwin")
    original = ["/bin/bash", "-c", "echo hi"]
    monkeypatch.setattr(local.os.path, "exists", lambda path: True)
    with patch("hermes_cli.config_effective.load_user_config_effective", return_value={
        "terminal": {"sandbox_macos_secrets": False},
    }):
        wrapped = local._sandbox_local_macos_argv(original)

    assert wrapped is original


def test_sandbox_setting_defaults_on():
    from hermes_cli.config_defaults import DEFAULT_CONFIG

    assert DEFAULT_CONFIG["terminal"]["sandbox_macos_secrets"] is True


def test_local_environment_spawn_uses_sandbox_wrapper(monkeypatch, tmp_path):
    monkeypatch.setattr(local.sys, "platform", "darwin")
    env = local.LocalEnvironment.__new__(local.LocalEnvironment)
    env.cwd = str(tmp_path)
    env.env = {}
    captured = {}

    monkeypatch.setattr(local, "_find_bash", lambda: "/bin/bash")
    monkeypatch.setattr(local, "_make_run_env", lambda _env: {})
    monkeypatch.setattr(env, "_recover_cwd", lambda: None)

    def fake_popen(args, **kwargs):
        captured["args"] = args
        class FakeProcess:
            pid = 1
        return FakeProcess()

    monkeypatch.setattr(local.subprocess, "Popen", fake_popen)
    monkeypatch.setattr(local, "own_process_groups", lambda _groups: None)
    monkeypatch.setattr(local.os.path, "exists", lambda path: path == "/usr/bin/sandbox-exec")
    with patch("hermes_cli.config_effective.load_user_config_effective", return_value={"terminal": {}}):
        with patch("gateway.status.get_process_start_time", return_value=None):
            env._run_bash("echo hi")

    assert captured["args"][:2] == ["/usr/bin/sandbox-exec", "-p"]
    assert captured["args"][-3:] == ["/bin/bash", "-c", "echo hi"]


@pytest.mark.parametrize("platform", ["darwin", "linux"])
@pytest.mark.parametrize("use_pty", [False, True])
def test_background_spawn_sandbox(platform, use_pty, monkeypatch, tmp_path):
    from types import SimpleNamespace
    from tools import process_registry as pr

    registry = pr.ProcessRegistry()
    session = pr.ProcessSession(id="proc_test", command="echo hi", task_id="test", cwd=str(tmp_path))
    captured = {}
    monkeypatch.setattr(local.sys, "platform", platform)
    monkeypatch.setattr(local.os.path, "exists", lambda path: path == "/usr/bin/sandbox-exec")
    monkeypatch.setattr(local, "_local_macos_secret_profile", lambda: "profile")
    monkeypatch.setattr(pr, "_find_shell", lambda: "/bin/bash")
    monkeypatch.setattr(pr, "_IS_LINUX", False)
    monkeypatch.setattr(pr, "worker_output_log", lambda _id: None)
    monkeypatch.setattr(registry, "_spawn_env", lambda _env: {})
    monkeypatch.setattr(registry, "_track_started", lambda *args: None)
    monkeypatch.setattr(registry, "_safe_host_start_time", lambda _pid: None)
    monkeypatch.setattr(registry, "_own_pty_job", lambda *_args: None)

    def capture(argv, **kwargs):
        captured["argv"] = argv
        return SimpleNamespace(pid=1234)

    monkeypatch.setattr(registry, "_spawn_job", lambda _session, argv, _persist, **kwargs: capture(argv, **kwargs))
    with patch("hermes_cli.config_effective.load_user_config_effective", return_value={"terminal": {}}):
        with patch("ptyprocess.PtyProcess.spawn", side_effect=capture):
            registry._spawn_local_admitted(session, "echo hi", {}, use_pty, False)

    shell = ["/bin/bash", "-lic", "set +m; echo hi"]
    expected = (["/usr/bin/sandbox-exec", "-p", "profile", "/usr/bin/env", "HERMES_SECRET_SANDBOX=1", *shell]
                if platform == "darwin" else shell)
    assert captured["argv"] == expected


def test_profile_covers_shared_credentials_root_and_writes(tmp_path, monkeypatch):
    import json
    from agent.file_safety import _CREDENTIAL_FILE_NAMES

    root = tmp_path / 'José " space'
    home = root / "profiles" / "nemo"
    monkeypatch.setenv("HERMES_HOME", str(home))
    profile = local._local_macos_secret_profile()
    for path in [*(home / name for name in _CREDENTIAL_FILE_NAMES), home / ".op.env",
                 home / ".rr-token", root / "auth.json"]:
        literal = json.dumps(str(path), ensure_ascii=False)
        assert f"(deny file-read* file-write* file-link (literal {literal}))" in profile
    for path in (home, *home.parents, home / "config.yaml"):
        assert f"(deny file-write* (literal {json.dumps(str(path), ensure_ascii=False)}))" in profile
    assert str(home / "logs") not in profile


def test_profile_denies_secret_directories_and_symlink_ancestors(tmp_path, monkeypatch):
    real = tmp_path / "real"
    real.mkdir()
    alias = tmp_path / "alias"
    alias.symlink_to(real, target_is_directory=True)
    home = alias / "profiles" / "nemo"
    monkeypatch.setenv("HERMES_HOME", str(home))
    profile = local._local_macos_secret_profile()
    for path in (home, *home.parents):
        for spelling in (path, path.resolve()):
            assert f'(deny file-write* (literal "{spelling}"))' in profile
    for name in ("mcp-tokens", "vault", "browser-profile", "webapp"):
        for path in (home / name, (home / name).resolve()):
            assert f'(deny file-read* file-write* file-link (subpath "{path}"))' in profile


def test_profile_rejects_control_characters(tmp_path, monkeypatch):
    monkeypatch.setenv("HERMES_HOME", str(tmp_path / "bad\x01home"))
    with pytest.raises(ValueError, match="control characters"):
        local._local_macos_secret_profile()


def test_config_error_still_wraps(monkeypatch):
    monkeypatch.setattr(local.sys, "platform", "darwin")
    monkeypatch.setattr(local.os.path, "exists", lambda path: True)
    monkeypatch.setattr(local, "_local_macos_secret_profile", lambda: "profile")
    with patch("hermes_cli.config_effective.load_user_config_effective", side_effect=OSError):
        assert local._sandbox_local_macos_argv(["shell"])[0:2] == ["/usr/bin/sandbox-exec", "-p"]


def test_kernel_spawn_uses_sandbox(monkeypatch, tmp_path):
    from types import SimpleNamespace
    from tools import code_kernel

    monkeypatch.setattr(local.sys, "platform", "darwin")
    monkeypatch.setattr(local.os.path, "exists", lambda path: path == "/usr/bin/sandbox-exec")
    monkeypatch.setattr(local, "_local_macos_secret_profile", lambda: "profile")
    monkeypatch.setattr(code_kernel.tempfile, "mkdtemp", lambda **kwargs: str(tmp_path))
    monkeypatch.setattr(code_kernel, "_bind_rpc_socket", lambda kernel: "socket")
    monkeypatch.setattr(code_kernel, "_ensure_background_reaper", lambda: None)
    monkeypatch.setattr(code_kernel.threading, "Thread", lambda **kwargs: SimpleNamespace(start=lambda: None))
    captured = {}
    def spawn(argv, **kwargs):
        captured["argv"] = argv
        return SimpleNamespace(pid=1234)
    monkeypatch.setattr("hermes_cli.process_identity.spawn_owned", spawn)
    monkeypatch.setattr("tools.code_execution_env._build_child_env", lambda **kwargs: {})
    monkeypatch.setattr("tools.code_execution_tool.generate_hermes_tools_module", lambda tools: "")
    with patch("hermes_cli.config_effective.load_user_config_effective", return_value={"terminal": {}}):
        code_kernel._spawn(code_kernel.SessionKernel(("test",)), child_python="/bin/python", child_cwd="",
                           sandbox_tools=frozenset(), max_tool_calls=1)
    assert captured["argv"] == ["/usr/bin/sandbox-exec", "-p", "profile", "/usr/bin/env",
                                "HERMES_SECRET_SANDBOX=1", "/bin/python", str(tmp_path / "hermes_kernel_runner.py")]

