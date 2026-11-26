"""Safe gateway state reset behavior."""

from types import SimpleNamespace

import pytest

from hermes_cli import gateway
from hermes_cli.state_reset import reset_state


def _state_home(tmp_path, monkeypatch):
    home = tmp_path / ".hermes"
    home.mkdir()
    monkeypatch.setattr("hermes_constants.get_hermes_home", lambda: home)
    return home


def _database_files(home):
    files = {name: home / name for name in ("state.db", "state.db-wal", "state.db-shm")}
    for name, path in files.items():
        path.write_text(name, encoding="utf-8")
    return files


def test_state_reset_keeps_database_when_stop_fails(tmp_path, monkeypatch, capsys):
    home = _state_home(tmp_path, monkeypatch)
    files = _database_files(home)

    def stop(_args):
        raise SystemExit(1)

    monkeypatch.setattr(gateway, "_cmd_stop", stop)
    monkeypatch.setattr(gateway, "_cmd_start", lambda _args: pytest.fail("must not start"))

    assert reset_state() == 1
    assert all(path.exists() for path in files.values())
    assert "state.db was not moved" in capsys.readouterr().out
    assert not list(home.glob("state.db.pre-reset-*"))


def test_state_reset_keeps_database_until_gateway_pid_is_gone(tmp_path, monkeypatch, capsys):
    home = _state_home(tmp_path, monkeypatch)
    files = _database_files(home)
    monkeypatch.setattr(gateway, "_cmd_stop", lambda _args: None)
    monkeypatch.setattr(gateway, "_cmd_start", lambda _args: pytest.fail("must not start"))
    monkeypatch.setattr(
        "gateway.status.resolve_gateway_liveness",
        lambda **kwargs: SimpleNamespace(running=True, pid=123, probe_error=False),
    )
    assert reset_state() == 1
    assert all(path.exists() for path in files.values())
    output = capsys.readouterr().out
    assert "PID 123 is still running" in output
    assert not list(home.glob("state.db.pre-reset-*"))


def test_state_reset_moves_database_and_marks_complete_after_start(tmp_path, monkeypatch, capsys):
    home = _state_home(tmp_path, monkeypatch)
    files = _database_files(home)
    calls = []
    monkeypatch.setattr(gateway, "_cmd_stop", lambda _args: calls.append("stop"))
    states = iter([
        SimpleNamespace(running=False, pid=None, probe_error=False),
        SimpleNamespace(running=False, pid=None, probe_error=False),
        SimpleNamespace(running=True, pid=456, probe_error=False),
    ])
    monkeypatch.setattr(
        "gateway.status.resolve_gateway_liveness", lambda **kwargs: next(states)
    )
    monkeypatch.setattr(gateway, "_cmd_start", lambda _args: calls.append("start"))

    assert reset_state() == 0

    backups = list(home.glob("state.db.pre-reset-*"))
    assert len(backups) == 1
    assert calls == ["stop", "start"]
    assert all((backups[0] / name).read_text(encoding="utf-8") == name for name in files.keys())
    assert all(not path.exists() for path in files.values())
    assert "State reset complete" in capsys.readouterr().out


def test_state_reset_leaves_backup_on_restart_failure(tmp_path, monkeypatch, capsys):
    home = _state_home(tmp_path, monkeypatch)
    _database_files(home)
    monkeypatch.setattr(gateway, "_cmd_stop", lambda _args: None)
    monkeypatch.setattr(
        "gateway.status.resolve_gateway_liveness",
        lambda **kwargs: SimpleNamespace(running=False, pid=None, probe_error=False),
    )
    monkeypatch.setattr(
        gateway, "_cmd_start", lambda _args: (_ for _ in ()).throw(OSError("launchd unavailable"))
    )

    assert reset_state() == 1
    assert "State reset complete" not in capsys.readouterr().out
    backups = list(home.glob("state.db.pre-reset-*"))
    assert len(backups) == 1
    assert (backups[0] / "state.db").exists()


def test_state_reset_retries_after_restart_failure(tmp_path, monkeypatch):
    home = _state_home(tmp_path, monkeypatch)
    _database_files(home)
    running = False
    monkeypatch.setattr(gateway, "_cmd_stop", lambda _args: None)
    monkeypatch.setattr(
        "gateway.status.resolve_gateway_liveness",
        lambda **kwargs: SimpleNamespace(running=running, pid=456 if running else None, probe_error=False),
    )

    def fail(_args):
        raise OSError("launchd unavailable")

    monkeypatch.setattr(gateway, "_cmd_start", fail)
    assert reset_state() == 1
    assert not (home / "reset.done").exists()
    backups = list(home.glob("state.db.pre-reset-*"))
    assert len(backups) == 1

    def start(_args):
        nonlocal running
        running = True

    monkeypatch.setattr(gateway, "_cmd_start", start)
    assert reset_state() == 0
    assert (home / "reset.done").exists()
    assert list(home.glob("state.db.pre-reset-*")) == backups
    assert reset_state() == 0


def test_state_reset_refuses_nonzero_stop_return(tmp_path, monkeypatch):
    home = _state_home(tmp_path, monkeypatch)
    files = _database_files(home)
    monkeypatch.setattr(gateway, "_cmd_stop", lambda _args: 1)
    monkeypatch.setattr(gateway, "_cmd_start", lambda _args: pytest.fail("must not start"))
    assert reset_state() == 1
    assert all(path.exists() for path in files.values())
    assert not (home / "reset.done").exists()


def test_state_reset_parser_registers_subcommand():
    from hermes_cli.main import _build_cli_parser

    parser, _subparsers = _build_cli_parser()
    args = parser.parse_args(["state", "reset"])
    assert args.state_action == "reset"
    assert args.func.__module__ == "hermes_cli.subcommands.state"
