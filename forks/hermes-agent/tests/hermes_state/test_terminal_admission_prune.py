"""Terminal admission rows of a live, unretired session are pruned through the retirement tombstones.

A row whose session has been idle past ``RETRY_HORIZON_S`` is replaced by its ``ADMISSION_PREFIX`` and
``identity_key`` tombstones, in the same transaction as its delete, so the duplicate guard still refuses
the same request. Queued, started and unknown rows, the session's latest row, and every row of a session
with live work stay in ``session_admissions``.
"""
import time

import pytest

import hermes_state_runtime as rt
from gateway.session_results import retain_result
from hermes_state import SessionDB

IDLE_DAYS = 8


def _settle(db, epoch, sid, request_id, text):
    args = dict(principal_id='api', session_id=sid, request_id=request_id, payload={'text': text})
    row = rt.admit_session_input(db, epoch=epoch, **args)
    claimed = rt.claim_session_input(db, epoch=epoch, session_id=sid)
    retain_result(db, epoch=epoch, row=claimed, result={'result': {'final_response': 'reply', 'messages': []}, 'usage': {}})
    return args, row['admission_id']


def _age(db, sid, days=IDLE_DAYS):
    old = time.time() - days * 86400
    db._execute_write(lambda conn: conn.execute(
        'UPDATE sessions SET started_at=?, last_activity_at=? WHERE id=?', (old, old, sid)))


def test_a_pruned_terminal_request_is_still_refused_as_a_duplicate_on_a_live_session(tmp_path):
    from hermes_state_terminal import ADMISSION_PREFIX
    with SessionDB(tmp_path / 'state.db') as db:
        epoch = rt.begin_runtime_epoch(db, instance_id='owner')
        db.create_session('live', source='api_server')
        first, first_id = _settle(db, epoch, 'live', 'r1', 'charge card')
        _settle(db, epoch, 'live', 'r2', 'second')
        _age(db, 'live')

        db.prune_sessions(older_than_days=90)

        assert db.get_session('live') is not None, 'the live session itself must survive the prune'
        assert db.get_meta(ADMISSION_PREFIX + first_id) is not None, 'pruned row left no tombstone'
        assert rt.find_session_admission(db, principal_id='api', session_id='live', request_id='r1') is None, \
            'terminal row of a live session was not pruned'
        replay = rt.admit_session_input(db, epoch=epoch, **first)
        assert replay['admission_id'] == first_id and replay['status'] == 'terminal'
        with pytest.raises(rt.RuntimeStoreError, match='admission_conflict'):
            rt.admit_session_input(db, epoch=epoch, **{**first, 'payload': {'text': 'other'}})
        assert [r['request_id'] for r in rt.list_session_admissions(db, session_id='live', pending_only=False)] == ['r2']


def test_the_prune_set_excludes_live_rows_and_the_latest_row(tmp_path):
    with SessionDB(tmp_path / 'state.db') as db:
        epoch = rt.begin_runtime_epoch(db, instance_id='owner')
        for sid in ('aged', 'busy', 'stuck'):
            db.create_session(sid, source='api_server')
        _settle(db, epoch, 'aged', 'a1', 'a1')
        _settle(db, epoch, 'aged', 'a2', 'a2')
        _settle(db, epoch, 'aged', 'a3', 'a3')
        _settle(db, epoch, 'busy', 'b1', 'b1')
        rt.admit_session_input(db, epoch=epoch, principal_id='api', session_id='busy', request_id='b2',
                               payload={'text': 'b2'})
        _settle(db, epoch, 'stuck', 's1', 's1')
        rt.admit_session_input(db, epoch=epoch, principal_id='api', session_id='stuck', request_id='s2',
                               payload={'text': 's2'})
        db._execute_write(lambda conn: conn.execute(
            "UPDATE session_admissions SET status='unknown', owner_epoch=?, generation=1 WHERE request_id='s2'",
            (epoch,)))
        for sid in ('aged', 'busy', 'stuck'):
            _age(db, sid)

        db.prune_sessions(older_than_days=90)

        assert rt.find_session_admission(db, principal_id='api', session_id='aged', request_id='a1') is None
        assert rt.find_session_admission(db, principal_id='api', session_id='aged', request_id='a2') is None
        assert rt.find_session_admission(db, principal_id='api', session_id='aged', request_id='a3') is not None, \
            'the session latest row must stay'
        assert rt.find_session_admission(db, principal_id='api', session_id='busy', request_id='b2') is not None, \
            'queued row must stay'
        assert rt.find_session_admission(db, principal_id='api', session_id='busy', request_id='b1') is not None, \
            'terminal row of a session with queued work must stay'
        assert rt.find_session_admission(db, principal_id='api', session_id='stuck', request_id='s2') is not None, \
            'unknown row must stay'
        assert rt.find_session_admission(db, principal_id='api', session_id='stuck', request_id='s1') is not None, \
            'terminal row of a session with unknown work must stay'
