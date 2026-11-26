"""Safe offline reset of the Hermes SQLite state database."""

from __future__ import annotations

import os
from datetime import UTC, datetime
from pathlib import Path

from agent.deadline import poll_until


def _backup_dir(home: Path) -> Path:
    stamp = datetime.now(UTC).strftime("%Y%m%d-%H%M%S-%f")
    return home / f"state.db.pre-reset-{stamp}"


def _move_database(home: Path, backup: Path) -> None:
    backup.mkdir(mode=0o700)
    moved: list[tuple[Path, Path]] = []
    try:
        for name in ("state.db", "state.db-wal", "state.db-shm"):
            source = home / name
            if not source.exists():
                continue
            target = backup / name
            os.replace(source, target)
            moved.append((target, source))
    except OSError as exc:
        for target, source in reversed(moved):
            try:
                os.replace(target, source)
            except OSError as rollback_error:
                raise OSError(
                    f"move failed ({exc}); rollback failed restoring {source} ({rollback_error})"
                ) from rollback_error
        backup.rmdir()
        raise


def _gateway_is_running(resolve_liveness, home: Path) -> bool:
    state = resolve_liveness(profile_dir=home, use_cache=False)
    return not state.probe_error and state.running


def reset_state() -> int:
    """Stop the gateway, move DB files, and start it again; preserve files on every failure."""
    from argparse import Namespace
    from hermes_constants import get_hermes_home
    from hermes_cli import gateway
    from gateway.status import resolve_gateway_liveness

    home = get_hermes_home()
    try:
        gateway._cmd_stop(Namespace(system=False, all=False, interrupt=False))
    except SystemExit as exc:
        print("State reset stopped: gateway stop was not confirmed; state.db was not moved.")
        return int(exc.code) if isinstance(exc.code, int) else 1
    except Exception as exc:
        print(f"State reset stopped: gateway stop failed; state.db was not moved ({exc}).")
        return 1

    try:
        if not poll_until(
            lambda: not _gateway_is_running(resolve_gateway_liveness, home), timeout=15.0, interval=0.2
        ):
            print("State reset stopped: gateway is still running; state.db was not moved.")
            return 1
    except Exception as exc:
        print(f"State reset stopped: could not confirm gateway exit; state.db was not moved ({exc}).")
        return 1

    stopped = resolve_gateway_liveness(profile_dir=home, use_cache=False)
    if stopped.running or stopped.probe_error:
        detail = f"PID {stopped.pid} is still running" if stopped.running else "gateway status is unknown"
        print(f"State reset stopped: {detail}; state.db was not moved.")
        return 1

    backup = _backup_dir(home)
    try:
        _move_database(home, backup)
    except OSError as exc:
        print(f"State reset stopped: could not move state.db and its SQLite sidecars ({exc}).")
        return 1

    try:
        result = gateway._cmd_start(Namespace(system=False, all=False, force=False))
        if isinstance(result, int) and result != 0:
            raise RuntimeError(f"gateway start returned {result}")
        if not poll_until(
            lambda: _gateway_is_running(resolve_gateway_liveness, home), timeout=10.0, interval=0.2
        ):
            raise RuntimeError("gateway start was not confirmed")
    except Exception as exc:
        print(f"State reset incomplete: database is backed up at {backup}; gateway restart failed ({exc}).")
        return 1

    print(f"State reset complete. Previous database files: {backup}")
    return 0


__all__ = ["_backup_dir", "_move_database", "reset_state"]
