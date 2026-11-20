"""Startup native recovery reads its pending set and route owners off the event loop.

``recover_profile_native_sessions`` ran a pending-row SELECT and one ``logical_owner`` lineage read per session
inline on the loop. Under a long session list that starves the Discord heartbeat (RIG-11272, 15:34 ET block).
"""

import asyncio
import threading
from contextlib import contextmanager
from datetime import datetime

import pytest

from gateway.config import Platform
from gateway.session import SessionEntry
from tests.gateway.restart_test_helpers import make_restart_runner, make_restart_source
from tests.gateway.test_restart_resume_pending import _restart_authority


def _routed_entry(index):
    source = make_restart_source(chat_id=f"recover-{index}")
    return SessionEntry(
        session_key=f"agent:main:telegram:dm:recover-{index}",
        session_id=f"recover-s{index}",
        created_at=datetime.now(),
        updated_at=datetime.now(),
        origin=source,
        platform=Platform.TELEGRAM,
        chat_type="dm",
    )


@pytest.mark.asyncio
async def test_slow_store_does_not_block_the_loop_during_native_recovery(tmp_path, monkeypatch):
    """The first store read of recovery asks the loop to set loop_ran. Off the loop, the wait returns at once;
    on the loop it times out (the red case)."""
    from gateway.run_runtime import recover_profile_native_sessions

    db, authority = _restart_authority(tmp_path, monkeypatch)
    entries = [_routed_entry(index) for index in range(25)]
    runner, _adapter = make_restart_runner()
    runner.session_authority = authority
    runner.session_store._entries = {entry.session_key: entry for entry in entries}

    loop = asyncio.get_running_loop()
    loop_ran = threading.Event()
    observed = []
    real_read_ctx = db._read_ctx

    @contextmanager
    def slow_read_ctx(*args, **kwargs):
        if not observed:
            loop.call_soon_threadsafe(loop_ran.set)
            observed.append(loop_ran.wait(timeout=5))
        with real_read_ctx(*args, **kwargs) as conn:
            yield conn

    monkeypatch.setattr(db, "_read_ctx", slow_read_ctx)
    with db:
        outcome = await recover_profile_native_sessions(runner, authority)
    assert observed == [True], "the native recovery read blocked the event loop: loop_ran was not set within 5 s"
    assert outcome == {}
