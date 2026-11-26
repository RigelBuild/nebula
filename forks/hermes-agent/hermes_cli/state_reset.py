"""Safe offline reset of the Hermes SQLite state database."""

from __future__ import annotations

import os
from datetime import UTC, datetime
from pathlib import Path


_DATABASE_FILES = ("state.db", "state.db-wal", "state.db-shm")


def _has_database(home: Path) -> bool:
    return any((home / name).exists() for name in _DATABASE_FILES)


def _backup_dir(home: Path) -> Path:
    stamp = datetime.now(UTC).strftime("%Y%m%d-%H%M%S-%f")
    base = home / f"state.db.pre-reset-{stamp}"
    candidate = base
    suffix = 2
    while candidate.exists():
        candidate = home / f"{base.name}-{suffix}"
        suffix += 1
    return candidate


def _pending_backup(home: Path) -> Path | None:
    for backup in sorted(home.glob("state.db.pre-reset-*"), reverse=True):
        if backup.is_dir() and any((backup / name).exists() for name in _DATABASE_FILES):
            return backup
    return None


def _move_database(home: Path, backup: Path) -> None:
    backup.mkdir(mode=0o700, exist_ok=False)
    moved: list[tuple[Path, Path]] = []
    try:
        for name in _DATABASE_FILES:
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
    if state.probe_error:
        raise RuntimeError("gateway status is unknown")
    return state.running


def _start_gateway(gateway, Namespace, resolve_liveness, home: Path) -> None:
    result = gateway._cmd_start(Namespace(system=False, all=False, force=False))
    if isinstance(result, int) and result != 0:
        raise RuntimeError(f"gateway start returned {result}")
    if not poll_until(
        lambda: _gateway_is_running(resolve_liveness, home), timeout=10.0, interval=0.2
    ):
        raise RuntimeError("gateway start was not confirmed")


def reset_state() -> int:
    """Stop the gateway, preserve its database, then start it with a fresh database."""
    from argparse import Namespace

    from hermes_constants import get_hermes_home
    from hermes_cli import gateway
    from gateway.status import resolve_gateway_liveness
    home = get_hermes_home()
    pending_backup = None if _has_database(home) else _pending_backup(home)
    was_running = False
    if pending_backup is not None:
        try:
            was_running = resolve_gateway_liveness(profile_dir=home, use_cache=False).running
        except Exception as exc:
            print(f"State reset stopped: could not confirm gateway status ({exc}).")
            return 1
        if was_running:
            try:
                gateway._cmd_stop(Namespace(system=False, all=False, interrupt=False))
            except SystemExit as exc:
                print(f"State reset incomplete: database is backed up at {pending_backup}; gateway stop failed.")
                return int(exc.code) if isinstance(exc.code, int) else 1
            except Exception as exc:
                print(f"State reset incomplete: database is backed up at {pending_backup}; gateway stop failed ({exc}).")
                return 1
    else:
        try:
            gateway._cmd_stop(Namespace(system=False, all=False, interrupt=False))
        except SystemExit as exc:
            print("State reset stopped: gateway stop was not confirmed; state.db was not moved.")
            return int(exc.code) if isinstance(exc.code, int) else 1
        except Exception as exc:
            print(f"State reset stopped: gateway stop failed; state.db was not moved ({exc}).")
            return 1
        try:
            gateway._cmd_stop(Namespace(system=False, all=False, interrupt=False))
        except SystemExit as exc:
            print("State reset stopped: gateway stop was not confirmed; state.db was not moved.")
            return int(exc.code) if isinstance(exc.code, int) else 1
        except Exception as exc:
            print(f"State reset stopped: gateway stop failed; state.db was not moved ({exc}).")
            return 1

    if pending_backup is None or was_running:
        try:
            if not poll_until(
                lambda: not _gateway_is_running(resolve_gateway_liveness, home), timeout=15.0, interval=0.2
            ):
                print("State reset stopped: gateway is still running; state.db was not moved.")
                return 1
        except Exception as exc:
            print(f"State reset stopped: could not confirm gateway exit; state.db was not moved ({exc}).")
            return 1

    try:
        stopped = resolve_gateway_liveness(profile_dir=home, use_cache=False)
    except Exception as exc:
        print(f"State reset stopped: could not confirm gateway exit; state.db was not moved ({exc}).")
        return 1
    if stopped.running or stopped.probe_error:
        detail = f"PID {stopped.pid} is still running" if stopped.running else "gateway status is unknown"
        print(f"State reset stopped: {detail}; state.db was not moved.")
        return 1

    backup = pending_backup or _backup_dir(home)
    if pending_backup is None and _has_database(home):
        try:
            _move_database(home, backup)
        except OSError as exc:
            print(f"State reset stopped: could not move state.db and its SQLite sidecars ({exc}).")
            return 1

    try:
        _start_gateway(gateway, Namespace, resolve_gateway_liveness, home)
    except SystemExit as exc:
        print(f"State reset incomplete: database is backed up at {backup}; gateway restart exited ({exc.code}).")
        return 1
    except Exception as exc:
        print(f"State reset incomplete: database is backed up at {backup}; gateway restart failed ({exc}).")
        return 1

    print(f"State reset complete. Previous database files: {backup}")
    return 0


__all__ = ["_backup_dir", "_move_database", "reset_state"]
