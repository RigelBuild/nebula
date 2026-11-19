"""``held_admissions`` matches ``holds_admission`` per id on the shapes the batch must get right.

Each shape here is one the batched query can answer wrongly: a target whose verdict depends on which row is
latest by ``seq``, a reset child whose admission owner is another id, and more ids than one chunk.
"""

import asyncio

from gateway.session_contract import GATEWAY_RESTART_CAUSE, SessionActor, SessionRef
from tests.gateway.test_restart_resume_pending import _interrupted_result, _restart_authority

_COMPLETED = {'final_response': 'done', 'completed': True}


def _admit(db, authority, sid, request_id):
    from hermes_state_runtime import admit_session_input
    return admit_session_input(db, epoch=authority.epoch, principal_id='person', session_id=sid,
                               request_id=request_id, payload={'text': request_id})


def _settle_head(db, authority, sid, outcome, result):
    from hermes_state_runtime import claim_session_input, settle_session_input
    row = claim_session_input(db, epoch=authority.epoch, session_id=sid)
    settle_session_input(db, epoch=authority.epoch, admission_id=row['admission_id'], generation=row['generation'],
                         outcome=outcome, result=result)


def test_latest_row_decides_not_the_lowest_seq(tmp_path, monkeypatch):
    """Two deciding rows on one target, with opposite verdicts in each order.

    ``older-interrupted``: an older restart-interrupted turn, then a newer completed turn. The newer completed
    turn decides, so the session holds. A lowest-``seq`` pick reads the interrupted row and releases it.
    ``older-completed``: an older completed turn, then a newer interrupted turn. The newer interrupted turn
    decides, so the session resumes (not held). A lowest-``seq`` pick reads the completed row and holds it."""
    from gateway.session_authorities import held_admissions, holds_admission
    db, authority = _restart_authority(tmp_path, monkeypatch)
    with db:
        for sid in ('older-interrupted', 'older-completed'):
            db.create_session(sid, source='test')
        _admit(db, authority, 'older-interrupted', 'oi1')
        _settle_head(db, authority, 'older-interrupted', 'interrupted', _interrupted_result(GATEWAY_RESTART_CAUSE))
        _admit(db, authority, 'older-interrupted', 'oi2')
        _settle_head(db, authority, 'older-interrupted', 'completed', _COMPLETED)
        _admit(db, authority, 'older-completed', 'oc1')
        _settle_head(db, authority, 'older-completed', 'completed', _COMPLETED)
        _admit(db, authority, 'older-completed', 'oc2')
        _settle_head(db, authority, 'older-completed', 'interrupted', _interrupted_result(GATEWAY_RESTART_CAUSE))
        ids = ['older-interrupted', 'older-completed']
        per_id = {sid for sid in ids if holds_admission(authority, sid)}
        batched = held_admissions(authority, ids)
    assert per_id == {'older-interrupted'}, "the newest decider decides: completed holds, interrupted resumes"
    assert batched == per_id


def test_reset_child_is_held_by_its_root_admission(tmp_path, monkeypatch):
    """A reset child answers to its root's admissions: ``admission_owner(child)`` is the root, not the child.

    The root is a real local session (it carries the local-policy receipt a reset reads), reset once so the
    child owns the transcript. The deciding row sits on the root. A batch that looks up only the child's own
    id finds nothing and releases the child, so the root's pending turn would be raced by a legacy resume."""
    from gateway.config import GatewayConfig
    from gateway.session import SessionStore
    from gateway.session_authorities import admission_owner, held_admissions, holds_admission
    from gateway.session_local import create_local_session
    from gateway.session_mutations import mutate_session
    db, authority = _restart_authority(tmp_path, monkeypatch)
    store = SessionStore(tmp_path / 'sessions', GatewayConfig())
    store._db = db
    authority.runner.session_store = store
    authority.runner._session_db = db
    authority.runner._adapter_for_source = lambda source: None
    authority.runner._evict_cached_agent = lambda route: None  # reset needs it (session_mutations.live_guard)
    authority.runner.adapters = {}
    authority.runner._draining = False
    actor = SessionActor('uid:1000', 'owned', frozenset({'session:create', 'session:read', 'session:submit',
                                                         'session:control'}), 'native')
    with db:
        ref = create_local_session(authority, actor, {'request_id': 'r', 'source': 'gui', 'cwd': str(tmp_path),
                                                      'model': 'fixture', 'toolsets': []})
        row = db.get_session(ref.session_id)
        reset = asyncio.run(mutate_session(authority, actor, ref, dict(
            session_id=ref.session_id, request_id='reset', expected_revision=row['runtime_revision'],
            expected_generation=row['runtime_generation'], operation='reset', payload={})))
        child = reset['target_session_id']
        assert child != ref.session_id
        root = admission_owner(authority, child)
        assert root != child
        # A pending admission on the root decides for the child (the owner lookup must reach the root).
        _admit(db, authority, root, 'root-q')
        assert holds_admission(authority, child) is True
        assert held_admissions(authority, [child]) == {child}
        _settle_head(db, authority, root, 'completed', _COMPLETED)
        # Two deciding rows across the (child, root) pair. The child's own row is older and completed; the
        # root's row is newer and interrupted. The newest row decides, so the child is not held. Reading the
        # pair's first row (the child's) would hold it.
        _admit(db, authority, child, 'child-old')
        _settle_head(db, authority, child, 'completed', _COMPLETED)
        _admit(db, authority, root, 'root-new')
        _settle_head(db, authority, root, 'interrupted', _interrupted_result(GATEWAY_RESTART_CAUSE))
        assert holds_admission(authority, child) is False
        assert held_admissions(authority, [child]) == set()


def test_many_ids_span_more_than_one_chunk(tmp_path, monkeypatch):
    """More ids than one query chunk: every chunk is read, and the held set is exact across the boundary."""
    from gateway.session_authorities import held_admissions, holds_admission
    db, authority = _restart_authority(tmp_path, monkeypatch)
    count = 2 * 400 + 5  # fixed, so a mutated HELD_ID_CHUNK fails the bound below instead of growing the test
    with db:
        ids = [f'bulk-{i:04d}' for i in range(count)]
        for sid in ids:
            db.create_session(sid, source='test')
        held_ids = ids[::7]  # every seventh session has a queued admission, the rest none
        for sid in held_ids:
            _admit(db, authority, sid, f'{sid}-q')
        per_id = {sid for sid in ids if holds_admission(authority, sid)}
        # Record how many targets each batched statement binds. Chunking must cap every statement at
        # HELD_ID_CHUNK, whatever the SQLite build's host-parameter limit is.
        bound: list[int] = []
        real_read_all = authority.db._read_all

        def counting_read_all(sql, params=(), *a, **kw):
            bound.append(len(params))
            return real_read_all(sql, params, *a, **kw)

        monkeypatch.setattr(authority.db, '_read_all', counting_read_all)
        batched = held_admissions(authority, ids)
    assert per_id == set(held_ids)
    assert batched == per_id
    assert bound and max(bound) <= 400, "a batched statement bound more targets than the 400-id chunk"
    assert len(bound) >= 2 * ((len(ids) + 400 - 1) // 400)
