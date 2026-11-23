"""The startup resume scan re-checks each candidate's live flags after the held-set read.

The candidates are snapshotted before the read runs in a worker thread, and the loop runs other work during
it. A ``/stop`` that suspends the session, or a turn that finishes and clears ``resume_pending``, must stop
that session being resumed, even though it was a candidate when the snapshot was taken.
"""

import asyncio
import threading
from unittest.mock import AsyncMock

import pytest

from tests.gateway.restart_test_helpers import make_restart_runner
from tests.gateway.test_restart_resume_pending import _restart_authority
from tests.gateway.test_startup_resume_admissions_off_loop import _candidate


async def _run_with_flip_during_read(tmp_path, monkeypatch, flip):
    """Start the async scan, and call *flip(entry)* from inside the worker-thread read.

    The flip happens on the worker thread while the loop waits on ``read_done``. The loop only resumes after
    the flip, so the scan's post-read check sees the live entry the flip changed."""
    db, authority = _restart_authority(tmp_path, monkeypatch)
    entry = _candidate(0)
    runner, adapter = make_restart_runner()
    runner.session_authority = authority
    runner.session_store._entries = {entry.session_key: entry}
    adapter.handle_message = AsyncMock()

    flipped = threading.Event()
    real_held = runner._held_resume_sessions

    def held_then_flip(authorities, candidates, *args):
        held = real_held(authorities, candidates, *args)
        flip(runner.session_store, entry)
        flipped.set()
        return held

    monkeypatch.setattr(runner, "_held_resume_sessions", held_then_flip)
    scheduled = await runner._schedule_resume_pending_sessions_async()
    for _ in range(5):
        await asyncio.sleep(0)
    assert flipped.wait(timeout=5), "the flip never ran inside the held-set read"
    return scheduled, adapter


@pytest.mark.asyncio
async def test_suspending_during_the_read_stops_the_resume(tmp_path, monkeypatch):
    """A ``/stop`` that suspends the session while the held set is read must not schedule its resume."""
    def suspend(store, entry):
        with store._lock:
            entry.suspended = True

    scheduled, adapter = await _run_with_flip_during_read(tmp_path, monkeypatch, suspend)
    assert scheduled == 0, "a session suspended during the read was resumed"
    adapter.handle_message.assert_not_called()


@pytest.mark.asyncio
async def test_a_turn_finishing_during_the_read_stops_the_resume(tmp_path, monkeypatch):
    """A turn that finishes while the held set is read clears ``resume_pending``; the scan must not resume it."""
    def finish(store, entry):
        with store._lock:
            entry.resume_pending = False
            entry.resume_reason = None
            entry.last_resume_marked_at = None

    scheduled, adapter = await _run_with_flip_during_read(tmp_path, monkeypatch, finish)
    assert scheduled == 0, "a session whose turn finished during the read was resumed again"
    adapter.handle_message.assert_not_called()
