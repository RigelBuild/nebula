"""Safe gateway state reset behavior."""

from types import SimpleNamespace

import pytest

from hermes_cli import gateway, state_reset
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


def _instant_poll(monkeypatch):
    monkeypatch.setattr(state_reset, "poll_until", lambda predicate, timeout, interval: predicate())


def _stopped(**kwargs):
    return SimpleNamespace(running=False, pid=None, probe_error=False, **kwargs)


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


def test_state_reset_keeps_database_when_stop_raises_exception(tmp_path, monkeypatch, capsys):
    home = _state_home(tmp_path, monkeypatch)
    files = _database_files(home)
    monkeypatch.setattr(gateway, "_cmd_stop", lambda _args: (_ for _ in ()).throw(OSError("stop failed")))
    monkeypatch.setattr(gateway, "_cmd_start", lambda _args: pytest.fail("must not start"))

    assert reset_state() == 1
    assert all(path.exists() for path in files.values())
    assert "gateway stop failed" in capsys.readouterr().out


def test_state_reset_keeps_database_until_gateway_pid_is_gone(tmp_path, monkeypatch, capsys):
    home = _state_home(tmp_path, monkeypatch)
    files = _database_files(home)
    _instant_poll(monkeypatch)
    monkeypatch.setattr(gateway, "_cmd_stop", lambda _args: None)
    monkeypatch.setattr(gateway, "_cmd_start", lambda _args: pytest.fail("must not start"))
    monkeypatch.setattr(
        "gateway.status.resolve_gateway_liveness",
        lambda **kwargs: SimpleNamespace(running=True, pid=123, probe_error=False),
    )

    assert reset_state() == 1
    assert all(path.exists() for path in files.values())
    assert "PID 123 is still running" in capsys.readouterr().out
    assert not list(home.glob("state.db.pre-reset-*"))


def test_state_reset_refuses_probe_error_during_stop_poll(tmp_path, monkeypatch, capsys):
    home = _state_home(tmp_path, monkeypatch)
    files = _database_files(home)
    _instant_poll(monkeypatch)
    monkeypatch.setattr(gateway, "_cmd_stop", lambda _args: None)
    monkeypatch.setattr(
        "gateway.status.resolve_gateway_liveness",
        lambda **kwargs: SimpleNamespace(running=False, pid=None, probe_error=True),
    )

    assert reset_state() == 1
    assert all(path.exists() for path in files.values())
    assert "could not confirm gateway exit" in capsys.readouterr().out


def test_state_reset_refuses_probe_error_at_final_check(tmp_path, monkeypatch, capsys):
    home = _state_home(tmp_path, monkeypatch)
    files = _database_files(home)
    _instant_poll(monkeypatch)
    states = iter([_stopped(), SimpleNamespace(running=False, pid=None, probe_error=True)])
    monkeypatch.setattr(gateway, "_cmd_stop", lambda _args: None)
    monkeypatch.setattr("gateway.status.resolve_gateway_liveness", lambda **kwargs: next(states))
    monkeypatch.setattr(gateway, "_cmd_start", lambda _args: pytest.fail("must not start"))

    assert reset_state() == 1
    assert all(path.exists() for path in files.values())
    assert "gateway status is unknown" in capsys.readouterr().out


def test_state_reset_refuses_gateway_that_appears_after_stop_before_move(tmp_path, monkeypatch, capsys):
    home = _state_home(tmp_path, monkeypatch)
    files = _database_files(home)
    monkeypatch.setattr(gateway, "_cmd_stop", lambda _args: None)
    states = iter([_stopped(), SimpleNamespace(running=True, pid=456, probe_error=False)])
    monkeypatch.setattr("gateway.status.resolve_gateway_liveness", lambda **kwargs: next(states))
    monkeypatch.setattr(gateway, "_cmd_start", lambda _args: pytest.fail("must not start"))
    _instant_poll(monkeypatch)

    assert reset_state() == 1
    assert all(path.exists() for path in files.values())
    assert "PID 456 is still running" in capsys.readouterr().out
    assert not list(home.glob("state.db.pre-reset-*"))


def test_state_reset_lock_refuses_respawn_between_stop_and_move(tmp_path, monkeypatch, capsys):
    from gateway.runtime_ownership import ProfileOwnership

    home = _state_home(tmp_path, monkeypatch)
    files = _database_files(home)
    monkeypatch.setattr(gateway, "_cmd_stop", lambda _args: None)
    monkeypatch.setattr("gateway.status.resolve_gateway_liveness", lambda **kwargs: _stopped())
    monkeypatch.setattr(gateway, "_cmd_start", lambda _args: pytest.fail("must not start"))
    respawn = ProfileOwnership()

    def poll(predicate, timeout, interval):
        assert predicate()
        if timeout == state_reset._STOP_TIMEOUT:
            respawn.reserve([home])
            return True
        return predicate()

    monkeypatch.setattr(state_reset, "poll_until", poll)
    try:
        assert reset_state() == 1
    finally:
        respawn.release(home)

    assert all(path.exists() for path in files.values())
    assert "gateway became active before its database could be moved" in capsys.readouterr().out
    assert not list(home.glob("state.db.pre-reset-*"))


def test_state_reset_moves_database_and_marks_complete_after_start(tmp_path, monkeypatch, capsys):
    home = _state_home(tmp_path, monkeypatch)
    files = _database_files(home)
    _instant_poll(monkeypatch)
    calls = []
    states = iter([_stopped(), _stopped(), SimpleNamespace(running=True, pid=456, probe_error=False)])
    monkeypatch.setattr(gateway, "_cmd_stop", lambda _args: calls.append("stop"))
    monkeypatch.setattr("gateway.status.resolve_gateway_liveness", lambda **kwargs: next(states))
    monkeypatch.setattr(gateway, "_cmd_start", lambda _args: calls.append("start"))

    assert reset_state() == 0

    backups = list(home.glob("state.db.pre-reset-*"))
    assert len(backups) == 1
    assert calls == ["stop", "start"]
    assert all((backups[0] / name).read_text(encoding="utf-8") == name for name in files)
    assert all(not path.exists() for path in files.values())
    assert "State reset complete" in capsys.readouterr().out


def test_move_database_rolls_back_after_move_failure(tmp_path, monkeypatch):
    home = tmp_path / ".hermes"
    home.mkdir()
    files = _database_files(home)
    backup = home / "state.db.pre-reset-test"
    replace = state_reset.os.replace

    def fail_sidecar(source, target):
        if Path(source).name == "state.db-wal" and Path(target).parent == backup:
            raise OSError("sidecar move failed")
        replace(source, target)

    monkeypatch.setattr(state_reset.os, "replace", fail_sidecar)
    with pytest.raises(OSError, match="sidecar move failed"):
        state_reset._move_database(home, backup)

    assert all(path.exists() for path in files.values())
    assert not backup.exists()


def test_move_database_reports_rollback_failure_and_preserves_split_state(tmp_path, monkeypatch):
    home = tmp_path / ".hermes"
    home.mkdir()
    files = _database_files(home)
    backup = home / "state.db.pre-reset-test"
    replace = state_reset.os.replace

    def fail_move_and_rollback(source, target):
        if Path(source).name == "state.db-wal" and Path(target).parent == backup:
            raise OSError("sidecar move failed")
        if Path(source).parent == backup and Path(source).name == "state.db":
            raise OSError("database rollback failed")
        replace(source, target)

    monkeypatch.setattr(state_reset.os, "replace", fail_move_and_rollback)
    with pytest.raises(OSError, match="rollback failed"):
        state_reset._move_database(home, backup)

    assert not files["state.db"].exists()
    assert (backup / "state.db").exists()
    assert files["state.db-wal"].exists() and files["state.db-shm"].exists()


def test_state_reset_refuses_split_state_after_rollback_failure(tmp_path, monkeypatch, capsys):
    home = _state_home(tmp_path, monkeypatch)
    files = _database_files(home)
    backup = home / "state.db.pre-reset-test"
    backup.mkdir()
    files["state.db"].rename(backup / "state.db")
    (home / "reset.pending").write_text(backup.name, encoding="utf-8")
    _instant_poll(monkeypatch)
    monkeypatch.setattr(gateway, "_cmd_stop", lambda _args: None)
    monkeypatch.setattr("gateway.status.resolve_gateway_liveness", lambda **kwargs: _stopped())
    monkeypatch.setattr(gateway, "_cmd_start", lambda _args: pytest.fail("must not start"))

    assert reset_state() == 1
    output = capsys.readouterr().out
    assert "incomplete state reset" in output
    assert str(backup / "state.db") in output
    assert files["state.db-wal"].exists() and files["state.db-shm"].exists()
    assert not (home / "reset.done").exists()


def test_state_reset_leaves_backup_on_restart_failure(tmp_path, monkeypatch, capsys):
    home = _state_home(tmp_path, monkeypatch)
    _database_files(home)
    _instant_poll(monkeypatch)
    monkeypatch.setattr(gateway, "_cmd_stop", lambda _args: None)
    monkeypatch.setattr("gateway.status.resolve_gateway_liveness", lambda **kwargs: _stopped())
    monkeypatch.setattr(
        gateway, "_cmd_start", lambda _args: (_ for _ in ()).throw(OSError("launchd unavailable"))
    )

    assert reset_state() == 1
    assert "State reset complete" not in capsys.readouterr().out
    backups = list(home.glob("state.db.pre-reset-*"))
    assert len(backups) == 1
    assert (backups[0] / "state.db").exists()
    assert not (home / "reset.done").exists()


def test_state_reset_does_not_mark_done_when_start_is_not_confirmed(tmp_path, monkeypatch, capsys):
    home = _state_home(tmp_path, monkeypatch)
    _database_files(home)
    _instant_poll(monkeypatch)
    states = iter([_stopped(), _stopped(), _stopped()])
    monkeypatch.setattr(gateway, "_cmd_stop", lambda _args: None)
    monkeypatch.setattr("gateway.status.resolve_gateway_liveness", lambda **kwargs: next(states))
    monkeypatch.setattr(gateway, "_cmd_start", lambda _args: None)

    assert reset_state() == 1
    assert "gateway start was not confirmed" in capsys.readouterr().out
    assert not (home / "reset.done").exists()


@pytest.mark.parametrize(
    "pending_name",
    ["", "/tmp/outside", "../outside", "nested/backup", "C:\\outside", "not-a-backup"],
)
def test_state_reset_refuses_invalid_pending_name(tmp_path, monkeypatch, capsys, pending_name):
    home = _state_home(tmp_path, monkeypatch)
    files = _database_files(home)
    (home / "reset.pending").write_text(pending_name, encoding="utf-8")
    _instant_poll(monkeypatch)
    monkeypatch.setattr(gateway, "_cmd_stop", lambda _args: None)
    monkeypatch.setattr("gateway.status.resolve_gateway_liveness", lambda **kwargs: _stopped())
    monkeypatch.setattr(gateway, "_cmd_start", lambda _args: pytest.fail("must not start"))

    assert reset_state() == 1
    assert "reset.pending must contain one relative backup name inside the Hermes home" in capsys.readouterr().out
    assert all(path.exists() for path in files.values())
    assert not (home / "reset.done").exists()


def test_state_reset_retries_after_restart_failure(tmp_path, monkeypatch):
    home = _state_home(tmp_path, monkeypatch)
    _database_files(home)
    _instant_poll(monkeypatch)
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


def test_state_reset_parser_registers_subcommand(capsys):
    from hermes_cli.main import _build_cli_parser

    parser, _subparsers = _build_cli_parser()
    args = parser.parse_args(["state", "reset"])
    assert args.state_action == "reset"
    assert args.func.__module__ == "hermes_cli.subcommands.state"

    with pytest.raises(SystemExit):
        parser.parse_args(["state", "reset", "--help"])
    assert "one-shot per home" in capsys.readouterr().out
