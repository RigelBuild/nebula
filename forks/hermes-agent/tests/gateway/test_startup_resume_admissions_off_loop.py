"""The startup resume scan reads its held set off the event loop, in one batched read per authority.

``_schedule_resume_pending_sessions`` called ``holds_admission`` inline for every candidate: blocking SQL
reads on the loop that serves the watchdog. The held set now comes from one worker-thread call.
"""

import asyncio
import sqlite3
import threading
from contextlib import contextmanager
from datetime import datetime
from unittest.mock import AsyncMock

import pytest

from gateway.config import Platform
from gateway.session import SessionEntry
from gateway.session_contract import GATEWAY_RESTART_CAUSE
from tests.gateway.restart_test_helpers import make_restart_runner, make_restart_source
from tests.gateway.test_restart_resume_pending import _interrupted_result, _restart_authority


def _candidate(index):
    source = make_restart_source(chat_id=f"bulk-{index}")
    return SessionEntry(
        session_key=f"agent:main:telegram:dm:bulk-{index}",
        session_id=f"bulk-s{index}",
        created_at=datetime.now(),
        updated_at=datetime.now(),
        origin=source,
        platform=Platform.TELEGRAM,
        chat_type="dm",
        resume_pending=True,
        resume_reason="restart_interrupted",
        last_resume_marked_at=datetime.now(),
    )


@pytest.mark.asyncio
async def test_slow_admissions_read_does_not_block_the_loop_during_startup_resume(tmp_path, monkeypatch):
    """Many candidates against a slow store: the loop keeps running while the startup scan reads the held set."""
    db, authority = _restart_authority(tmp_path, monkeypatch)
    entries = [_candidate(index) for index in range(25)]
    runner, adapter = make_restart_runner()
    runner.session_authority = authority
    runner.session_store._entries = {entry.session_key: entry for entry in entries}
    adapter.handle_message = AsyncMock()

    loop = asyncio.get_running_loop()
    loop_ran = threading.Event()
    observed = []
    real_read_ctx = db._read_ctx

    @contextmanager
    def slow_read_ctx(*args, **kwargs):
        # The first store read asks the loop to set loop_ran. The loop runs that callback only while this
        # read is off the loop, so the wait returns at once off the loop and times out on it (the red case).
        if not observed:
            loop.call_soon_threadsafe(loop_ran.set)
            observed.append(loop_ran.wait(timeout=5))
        with real_read_ctx(*args, **kwargs) as conn:
            yield conn

    monkeypatch.setattr(db, "_read_ctx", slow_read_ctx)
    with db:
        scheduled = await runner._schedule_resume_pending_sessions_async()
        for _ in range(5):
            await asyncio.sleep(0)
    assert observed == [True], "the admissions read blocked the event loop: loop_ran was not set within 5 s"
    assert scheduled == len(entries)


def _seed_store(db, authority):
    """One session per held-check shape, plus an absent id. Returns every candidate id, in order."""
    from hermes_state_runtime import (admit_session_input, begin_runtime_epoch, claim_session_input,
                                      recover_session_inputs, settle_session_input, steer_session_input)
    sids = ['unknown-s', 'queued-s', 'started-s', 'terminal-s', 'restart-s', 'steer-held-s', 'steer-resume-s',
            'absent-s']
    for sid in sids[:-1]:
        db.create_session(sid, source='test')
    epoch = authority.epoch

    def admit(sid, request_id):
        return admit_session_input(db, epoch=epoch, principal_id='person', session_id=sid,
                                   request_id=request_id, payload={'text': request_id})

    def settle(sid, outcome, result):
        row = claim_session_input(db, epoch=epoch, session_id=sid)
        settle_session_input(db, epoch=epoch, admission_id=row['admission_id'], generation=row['generation'],
                             outcome=outcome, result=result)

    # unknown: claimed under the launch epoch, then a restart's recovery marks it unknown.
    admit('unknown-s', 'u1')
    claim_session_input(db, epoch=epoch, session_id='unknown-s')
    epoch = begin_runtime_epoch(db, instance_id='restart')
    recover_session_inputs(db, epoch=epoch)
    admit('queued-s', 'q1')
    admit('started-s', 'st1')
    claim_session_input(db, epoch=epoch, session_id='started-s')
    admit('terminal-s', 't1')
    settle('terminal-s', 'completed', {'final_response': 'done', 'completed': True})
    admit('restart-s', 'r1')
    settle('restart-s', 'interrupted', _interrupted_result(GATEWAY_RESTART_CAUSE))
    # A steer rides on its head's claim: the head decides. A completed head holds; an interrupted one resumes.
    for head_sid, outcome, result in (
            ('steer-held-s', 'completed', {'final_response': 'done', 'completed': True}),
            ('steer-resume-s', 'interrupted', _interrupted_result(GATEWAY_RESTART_CAUSE))):
        head = admit(head_sid, f'{head_sid}-head')
        head = claim_session_input(db, epoch=epoch, session_id=head_sid)
        follower = admit(head_sid, f'{head_sid}-follow')
        steer_session_input(db, epoch=epoch, admission_id=follower['admission_id'], generation=head['generation'])
        settle_session_input(db, epoch=epoch, admission_id=head['admission_id'], generation=head['generation'],
                             outcome=outcome, result=result)
    return sids


def test_batched_held_set_matches_per_id_holds_admission(tmp_path, monkeypatch):
    """Parity: on a store with queued, started, unknown, terminal, steer and restart-interrupted rows, the
    batched set equals ``holds_admission`` per id."""
    from gateway.session_authorities import held_admissions, holds_admission
    db, authority = _restart_authority(tmp_path, monkeypatch)
    with db:
        sids = _seed_store(db, authority)
        per_id = {sid for sid in sids if holds_admission(authority, sid)}
        batched = held_admissions(authority, sids)
    assert batched == per_id
    assert per_id == {'unknown-s', 'queued-s', 'started-s', 'terminal-s', 'steer-held-s'}


def test_retiring_store_that_cannot_answer_holds_every_candidate(tmp_path, monkeypatch):
    """Fail closed: a parked profile's retained store that cannot read holds every candidate, as
    ``holds_admission`` does per id."""
    from gateway.session_authorities import held_admissions
    db, authority = _restart_authority(tmp_path, monkeypatch)
    authority.retiring = True

    def unreadable(*_args, **_kwargs):
        raise RuntimeError("store unreadable")

    monkeypatch.setattr(db, "_read_ctx", unreadable)
    with db:
        assert held_admissions(authority, ['a-s', 'b-s']) == {'a-s', 'b-s'}


def test_live_store_that_cannot_answer_raises(tmp_path, monkeypatch):
    """A serving (non-retiring) store that cannot read raises, as ``holds_admission`` does per id."""
    from gateway.session_authorities import held_admissions
    db, authority = _restart_authority(tmp_path, monkeypatch)

    def unreadable(*_args, **_kwargs):
        raise sqlite3.OperationalError("unreadable")

    monkeypatch.setattr(db, "_read_ctx", unreadable)
    with db, pytest.raises(sqlite3.OperationalError):
        held_admissions(authority, ['a-s'])
