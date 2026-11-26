"""Safe offline reset of the Hermes SQLite state database."""

from __future__ import annotations

import os
from datetime import UTC, datetime
from pathlib import Path, PureWindowsPath

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


def _move_database(home: Path, backup: Path, *, allow_existing_empty: bool = False) -> bool:
    backup.mkdir(mode=0o700, exist_ok=allow_existing_empty)
    if allow_existing_empty and any(backup.iterdir()):
        raise FileExistsError(f"backup directory is not empty: {backup}")
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
    return bool(moved)


def _gateway_is_running(resolve_liveness, home: Path) -> bool:
    state = resolve_liveness(profile_dir=home, use_cache=False)
    if state.probe_error:
        raise RuntimeError("gateway status is unknown")
    return state.running


_STOP_TIMEOUT = 15.0
_START_TIMEOUT = 10.0
_POLL_INTERVAL = 0.2


def _start_gateway(gateway, Namespace, resolve_liveness, home: Path) -> None:
    result = gateway._cmd_start(Namespace(system=False, all=False, force=False))
    if isinstance(result, int) and result != 0:
        raise RuntimeError(f"gateway start returned {result}")
    if not poll_until(
        lambda: _gateway_is_running(resolve_liveness, home), timeout=_START_TIMEOUT, interval=_POLL_INTERVAL
    ):
        raise RuntimeError("gateway start was not confirmed")


def _validated_backup(home: Path, pending: Path) -> Path:
    name = pending.read_text(encoding="utf-8").strip()
    relative = Path(name)
    windows_relative = PureWindowsPath(name)
    if (
        not name.startswith("state.db.pre-reset-")
        or relative.is_absolute()
        or windows_relative.is_absolute()
        or windows_relative.drive
        or len(relative.parts) != 1
        or len(windows_relative.parts) != 1
    ):
        raise ValueError("reset.pending must contain one relative state.db.pre-reset backup name inside the Hermes home")
    backup = home / relative
    if backup.resolve().parent != home.resolve():
        raise ValueError("reset.pending backup must resolve inside the Hermes home")
    return backup


def reset_state() -> int:
    """Stop the gateway, preserve its database, then start it with a fresh database."""
    from argparse import Namespace

    from hermes_constants import get_hermes_home
    from hermes_cli import gateway
    from gateway.runtime_ownership import OwnershipConflict, exclusive_maintenance
    from gateway.status import resolve_gateway_liveness

    home = get_hermes_home()
    done = home / "reset.done"
    pending = home / "reset.pending"
    if done.exists():
        print(f"State reset already completed ({done}). Delete this marker to reset again.")
        return 0
    if not pending.exists() and not _has_database(home):
        print(f"No state database files were found in the Hermes home: {home}; nothing to reset.")
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
            lambda: not _gateway_is_running(resolve_gateway_liveness, home),
            timeout=_STOP_TIMEOUT,
            interval=_POLL_INTERVAL,
        ):
            stopped = resolve_gateway_liveness(profile_dir=home, use_cache=False)
            print(f"State reset stopped: PID {stopped.pid} is still running; state.db was not moved.")
            return 1
    except Exception as exc:
        print(f"State reset stopped: could not confirm gateway exit; state.db was not moved ({exc}).")
        return 1

    try:
        with exclusive_maintenance([home]):
            stopped = resolve_gateway_liveness(profile_dir=home, use_cache=False)
            if stopped.probe_error:
                print("State reset stopped: gateway status is unknown; state.db was not moved.")
                return 1
            if stopped.running:
                print(f"State reset stopped: PID {stopped.pid} is still running; state.db was not moved.")
                return 1

            if pending.exists():
                backup = _validated_backup(home, pending)
            else:
                if not _has_database(home):
                    raise RuntimeError(f"no state database files were found in the Hermes home: {home}")
                backup = _backup_dir(home)
                pending.write_text(backup.name, encoding="utf-8")
            if backup.exists():
                has_backup_state = (backup / "state.db").exists()
                has_home_database = _has_database(home)
                if has_backup_state and not (home / "state.db").exists() and has_home_database:
                    remaining = ", ".join(
                        str(home / name)
                        for name in ("state.db-wal", "state.db-shm")
                        if (home / name).exists()
                    )
                    raise RuntimeError(
                        f"incomplete state reset: backup contains {backup / 'state.db'} "
                        f"while SQLite sidecars remain in the Hermes home ({remaining}); "
                        "preserve both locations and resolve the split state before retrying"
                    )
                if not has_backup_state:
                    if has_home_database:
                        if not _move_database(home, backup, allow_existing_empty=True):
                            raise RuntimeError(f"no state database files were moved into {backup}")
                    else:
                        raise RuntimeError(f"incomplete state reset: backup directory is empty: {backup}")
            elif _has_database(home):
                if not _move_database(home, backup):
                    raise RuntimeError(f"no state database files were moved into {backup}")
            else:
                raise RuntimeError(f"no state database files were found in the Hermes home: {home}")
    except OwnershipConflict as exc:
        print(f"State reset stopped: gateway became active before its database could be moved ({exc}).")
        return 1
    except (OSError, UnicodeError, ValueError, RuntimeError) as exc:
        print(f"State reset stopped: could not safely move state.db and its SQLite sidecars ({exc}).")
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


__all__ = ["reset_state"]
