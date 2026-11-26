"""Safe offline reset of the Hermes SQLite state database."""

from __future__ import annotations

import os
from datetime import UTC, datetime
from pathlib import Path

from agent.deadline import poll_until


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
    done = home / "reset.done"
    pending = home / "reset.pending"
    if done.exists():
        print("State reset already completed.")
        return 0
    try:
        result = gateway._cmd_stop(Namespace(system=False, all=False, interrupt=False))
        if isinstance(result, int) and result != 0:
            raise RuntimeError(f"gateway stop returned {result}")
    except SystemExit as exc:
        print("State reset stopped: gateway stop was not confirmed; state.db was not moved.")
        return exc.code if isinstance(exc.code, int) and exc.code != 0 else 1
    except Exception as exc:
        print(f"State reset stopped: gateway stop failed; state.db was not moved ({exc}).")
        return 1

    try:
        if not poll_until(
            lambda: not _gateway_is_running(resolve_gateway_liveness, home), timeout=15.0, interval=0.2
        ):
            stopped = resolve_gateway_liveness(profile_dir=home, use_cache=False)
            print(f"State reset stopped: PID {stopped.pid} is still running; state.db was not moved.")
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

    try:
        if pending.exists():
            backup = home / pending.read_text(encoding="utf-8").strip()
        else:
            backup = _backup_dir(home)
            pending.write_text(backup.name, encoding="utf-8")
        if not backup.exists() and _has_database(home):
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
    try:
        done.write_text(f"{backup.name}\n", encoding="utf-8")
        pending.unlink(missing_ok=True)
    except OSError as exc:
        print(f"State reset incomplete: could not record completion ({exc}).")
        return 1


    print(f"State reset complete. Previous database files: {backup}")
    return 0


__all__ = ["_backup_dir", "_move_database", "reset_state"]
