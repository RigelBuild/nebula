"""Terminal admission rows of a live, unretired session are pruned through the retirement tombstones.

A row whose session has been idle past ``RETRY_HORIZON_S`` is replaced by its ``ADMISSION_PREFIX`` and
``identity_key`` tombstones, in the same transaction as its delete, so the duplicate guard still refuses
the same request. Queued, started and unknown rows, the session's latest row, and every row of a session
with live work stay in ``session_admissions``.
"""
import json
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


def test_a_pruned_live_request_stays_refused_after_the_second_horizon(tmp_path):
    from hermes_state_terminal import ADMISSION_PREFIX, identity_key
    with SessionDB(tmp_path / 'state.db') as db:
        epoch = rt.begin_runtime_epoch(db, instance_id='owner')
        db.create_session('live', source='api_server')
        first, first_id = _settle(db, epoch, 'live', 'r1', 'charge card')
        _settle(db, epoch, 'live', 'r2', 'second')
        _age(db, 'live')
        db.prune_sessions(older_than_days=90)

        # Age the first pass's tombstone past the retry horizon, then sweep again.
        old = time.time() - IDLE_DAYS * 86400
        db._execute_write(lambda conn: conn.execute(
            "UPDATE state_meta SET value=json_set(value, '$.retired_at', ?) WHERE key=?",
            (old, ADMISSION_PREFIX + first_id)))
        db.prune_sessions(older_than_days=90)

        assert db.get_meta(identity_key('api', 'live', 'r1')) is not None, 'the identity pointer was reclaimed'
        with pytest.raises(rt.RuntimeStoreError, match='not_found'):
            rt.admit_session_input(db, epoch=epoch, **first)


def test_rows_age_by_the_compression_tip_not_the_lineage_root(tmp_path):
    """A compressed conversation's rows age by its tip (the continuation the user resumes), not its root.
    An idle tip prunes its root's rows; an active tip keeps them."""
    with SessionDB(tmp_path / 'state.db') as db:
        epoch = rt.begin_runtime_epoch(db, instance_id='owner')
        db.create_session('idle_root', source='api_server')
        _settle(db, epoch, 'idle_root', 'i1', 'i1')
        _settle(db, epoch, 'idle_root', 'i2', 'i2')
        db.create_session('idle_tip', source='api_server', parent_session_id='idle_root')
        db.create_session('busy_root', source='api_server')
        _settle(db, epoch, 'busy_root', 'b1', 'b1')
        _settle(db, epoch, 'busy_root', 'b2', 'b2')
        db.create_session('busy_tip', source='api_server', parent_session_id='busy_root')
        db._execute_write(lambda conn: conn.execute(
            "UPDATE sessions SET end_reason='compression' WHERE id IN ('idle_root', 'busy_root')"))
        _age(db, 'idle_root')
        _age(db, 'idle_tip')
        _age(db, 'busy_root')

        db.prune_sessions(older_than_days=90)

        assert rt.find_session_admission(db, principal_id='api', session_id='idle_root', request_id='i1') is None, \
            'idle tip: its root row should be pruned'
        assert rt.find_session_admission(db, principal_id='api', session_id='busy_root', request_id='b1') is not None, \
            'active tip: its root row must stay'


def test_one_pass_prunes_at_most_the_batch_limit(tmp_path, monkeypatch):
    """The prune is bounded: one pass removes at most the batch limit of aged rows; the rest wait."""
    import hermes_state_mutation_retirement as retirement
    monkeypatch.setattr(retirement, 'PRUNE_BATCH_LIMIT', 1)
    with SessionDB(tmp_path / 'state.db') as db:
        epoch = rt.begin_runtime_epoch(db, instance_id='owner')
        db.create_session('batch', source='api_server')
        for k in ('k1', 'k2', 'k3', 'k4'):
            _settle(db, epoch, 'batch', k, k)
        _age(db, 'batch')

        db.prune_sessions(older_than_days=90)

        left = len(rt.list_session_admissions(db, session_id='batch', pending_only=False))
        assert left == 3, f'expected 3 rows left after a limit-1 pass, got {left}'


def test_a_prune_between_the_read_and_the_write_cannot_readmit_a_pruned_request(tmp_path, monkeypatch):
    """Race: the retry's read finds no pointer (r1 is still a live row), then a prune commits before the
    admission write. The write must find the new pointer and refuse, never admit r1 as new."""
    import hermes_state_terminal as terminal
    with SessionDB(tmp_path / 'state.db') as db:
        epoch = rt.begin_runtime_epoch(db, instance_id='owner')
        db.create_session('live', source='api_server')
        first, _ = _settle(db, epoch, 'live', 'r1', 'charge card')
        _settle(db, epoch, 'live', 'r2', 'second')
        _age(db, 'live', days=8)

        real_retry = terminal.retry_terminal_admission

        def read_then_prune_commits(*args, **kwargs):
            result = real_retry(*args, **kwargs)  # reads: r1 is live, no pointer -> None
            db.prune_sessions(older_than_days=7)  # commits: r1 tombstoned, pointer written
            return result

        monkeypatch.setattr(terminal, 'retry_terminal_admission', read_then_prune_commits)
        out = rt.admit_session_input(db, epoch=epoch, **first)
        assert out['status'] == 'terminal', f'the pruned request was re-admitted as {out["status"]}'
        live_r1 = [r for r in rt.list_session_admissions(db, session_id='live', pending_only=False)
                   if r['request_id'] == 'r1']
        assert live_r1 == [], 'the pruned request left a live admission row'


def test_two_targets_over_the_limit_finish_on_the_next_pass(tmp_path, monkeypatch):
    """Paging by target: a limit of 2 across two aged sessions of three rows each takes two passes to
    finish. Each pass prunes at most the limit; the second pass completes the work."""
    import hermes_state_mutation_retirement as retirement
    monkeypatch.setattr(retirement, 'PRUNE_BATCH_LIMIT', 2)
    with SessionDB(tmp_path / 'state.db') as db:
        epoch = rt.begin_runtime_epoch(db, instance_id='owner')
        for sid in ('p', 'q'):
            db.create_session(sid, source='api_server')
            for k in ('k1', 'k2', 'k3'):
                _settle(db, epoch, sid, k, k)
            _age(db, sid)

        with db._read_ctx() as conn:
            first = retirement.prunable_terminal_admissions(conn)
        assert len(first) == 2, f'a pass prunes at most the limit, got {len(first)}'
        db.prune_sessions(older_than_days=90)
        left = sum(len(rt.list_session_admissions(db, session_id=s, pending_only=False)) for s in ('p', 'q'))
        assert left == 4, f'after pass one: 2 latest rows + 2 unpruned, got {left}'

        db.prune_sessions(older_than_days=90)
        left = sum(len(rt.list_session_admissions(db, session_id=s, pending_only=False)) for s in ('p', 'q'))
        assert left == 2, f'after pass two only the two latest rows should remain, got {left}'


def test_an_orphaned_pointer_is_reclaimed_when_its_session_is_gone(tmp_path):
    """A pointer whose session row no longer exists is reclaimed by the prune, however the session was
    retired. The session is deleted through the sanctioned path, which fences it; the orphaned pointer
    must still go once its admission tombstone ages out."""
    from hermes_state_terminal import identity_key
    from hermes_state_mutation_retirement import prune_retired_tombstones
    with SessionDB(tmp_path / 'state.db') as db:
        epoch = rt.begin_runtime_epoch(db, instance_id='owner')
        db.create_session('gone', source='api_server')
        _, first_id = _settle(db, epoch, 'gone', 'r1', 'charge card')
        _settle(db, epoch, 'gone', 'r2', 'second')
        _age(db, 'gone')
        db.prune_sessions(older_than_days=90)
        assert db.get_meta(identity_key('api', 'gone', 'r1')) is not None
        db.delete_session('gone')
        assert db.get_session('gone') is None
        # A pointer written before pointers recorded their session (delete cannot find it), and no
        # retired marker (an expired sweep marker): only the session-row check can reclaim it.
        db._execute_write(lambda conn: conn.execute(
            'INSERT OR REPLACE INTO state_meta(key,value) VALUES(?,?)',
            (identity_key('api', 'gone', 'r1'), json.dumps(first_id))))
        db._execute_write(lambda conn: conn.execute(
            'DELETE FROM state_meta WHERE key=?', ('gateway.retired_session.v1.gone',)))
        old = time.time() - IDLE_DAYS * 86400
        db._execute_write(lambda conn: conn.execute(
            "UPDATE state_meta SET value=json_set(value, '$.retired_at', ?) WHERE key=?",
            (old, 'gateway.terminal_admission.v1.' + first_id)))
        db._execute_write(lambda conn: prune_retired_tombstones(conn))
        assert db.get_meta(identity_key('api', 'gone', 'r1')) is None, 'orphaned pointer was not reclaimed'


def test_a_pass_walks_at_most_limit_targets_and_never_a_single_row_session(tmp_path, monkeypatch):
    """Candidate targets are chosen in SQL before any tip walk: a session with one terminal row has
    nothing to prune and is never walked, and one pass walks at most *limit* targets."""
    import hermes_state_mutation_retirement as retirement
    walks = []
    real_walk = retirement._tip_last_active
    monkeypatch.setattr(retirement, '_tip_last_active', lambda conn, sid: walks.append(sid) or real_walk(conn, sid))
    with SessionDB(tmp_path / 'state.db') as db:
        epoch = rt.begin_runtime_epoch(db, instance_id='owner')
        for n in range(30):
            sid = f'one{n:02d}'
            db.create_session(sid, source='api_server')
            _settle(db, epoch, sid, 'r1', 'r1')
            _age(db, sid)
        for n in range(6):
            sid = f'two{n:02d}'
            db.create_session(sid, source='api_server')
            _settle(db, epoch, sid, 'r1', 'r1')
            _settle(db, epoch, sid, 'r2', 'r2')
        with db._read_ctx() as conn:
            retirement.prunable_terminal_admissions(conn, limit=4)
        assert not [sid for sid in walks if sid.startswith('one')], 'a single-row session was walked'
        assert len(walks) <= 4, f'one pass walked {len(walks)} targets for a limit of 4'


def _prune_then_age_tombstone(db, epoch, sid):
    from hermes_state_terminal import ADMISSION_PREFIX
    first, first_id = _settle(db, epoch, sid, 'r1', 'charge card')
    _settle(db, epoch, sid, 'r2', 'second')
    _age(db, sid)
    db.prune_sessions(older_than_days=90)
    old = time.time() - IDLE_DAYS * 86400
    db._execute_write(lambda conn: conn.execute(
        "UPDATE state_meta SET value=json_set(value, '$.retired_at', ?) WHERE key=?",
        (old, ADMISSION_PREFIX + first_id)))
    db.prune_sessions(older_than_days=90)
    assert db.get_meta(ADMISSION_PREFIX + first_id) is None, 'the tombstone did not age out'
    return first


def test_deleting_a_session_reclaims_a_pointer_whose_tombstone_aged_out(tmp_path):
    from hermes_state_terminal import identity_key
    with SessionDB(tmp_path / 'state.db') as db:
        epoch = rt.begin_runtime_epoch(db, instance_id='owner')
        db.create_session('live', source='api_server')
        _prune_then_age_tombstone(db, epoch, 'live')
        assert db.get_meta(identity_key('api', 'live', 'r1')) is not None
        db.delete_session('live')
        db.prune_sessions(older_than_days=90)
        assert db.get_meta(identity_key('api', 'live', 'r1')) is None, 'the pointer outlived its session'


def test_a_recreated_swept_id_admits_its_old_request_id_again(tmp_path):
    from hermes_state_mutation_retirement import RETIRED_PREFIX
    with SessionDB(tmp_path / 'state.db') as db:
        epoch = rt.begin_runtime_epoch(db, instance_id='owner')
        db.create_session('named', source='api_server')
        first = _prune_then_age_tombstone(db, epoch, 'named')
        db.end_session('named', 'user_exit')
        _age(db, 'named', days=100)
        assert db.prune_sessions(older_than_days=90) == 1, 'the ended session was not swept'
        old = time.time() - 100 * 86400
        db._execute_write(lambda conn: conn.execute(
            "UPDATE state_meta SET value=json_set(value, '$.retired_at', ?) WHERE key=?",
            (old, RETIRED_PREFIX + 'named')))
        db.prune_sessions(older_than_days=90)
        assert db.get_meta(RETIRED_PREFIX + 'named') is None, 'the sweep marker did not age out'
        db.create_session('named', source='api_server')
        out = rt.admit_session_input(db, epoch=epoch, **first)
        assert out['status'] == 'queued', f'the reused id refused its old request id: {out}'
