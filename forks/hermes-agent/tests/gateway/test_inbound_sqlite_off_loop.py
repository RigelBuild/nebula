"""The inbound admission and /stop paths read session SQLite rows on a worker thread, not the loop."""
import threading
from types import SimpleNamespace

import pytest


@pytest.mark.asyncio
async def test_admit_message_reads_the_admission_row_off_the_loop(monkeypatch):
    import hermes_state_runtime
    from gateway import session_ingress

    loop_thread = threading.get_ident()
    reads = []

    def read_admission(db, *, admission_id):
        reads.append((admission_id, threading.get_ident()))
        return {'status': 'terminal', 'outcome': 'completed'}

    async def admit_native(event):
        return SimpleNamespace(status='queued', admission_id='a1')

    monkeypatch.setattr(hermes_state_runtime, 'get_session_admission', read_admission)
    authority = SimpleNamespace(db=object(), admit_native=admit_native, native_waiters=set(), waiters={})

    assert await session_ingress.admit_message(authority, SimpleNamespace(source=None)) is None
    assert [admission for admission, _ in reads] == ['a1']
    assert reads[0][1] != loop_thread, 'the admission row was read on the event loop thread'

@pytest.mark.asyncio
async def test_admit_message_returns_reply_settled_during_admission_read(monkeypatch):
    import asyncio
    import hermes_state_runtime
    from gateway import session_ingress

    loop = asyncio.get_running_loop()
    settled = threading.Event()

    def read_admission(db, *, admission_id):
        waiter = authority.waiters[admission_id]

        def settle():
            waiter.set_result('reply')
            settled.set()

        loop.call_soon_threadsafe(settle)
        assert settled.wait(timeout=2), 'the waiter was not settled during the status read'
        return {'status': 'terminal', 'outcome': 'completed'}

    async def admit_native(event):
        return SimpleNamespace(status='queued', admission_id='a2', ref=SimpleNamespace(session_id='s'))

    monkeypatch.setattr(hermes_state_runtime, 'get_session_admission', read_admission)
    authority = SimpleNamespace(db=object(), admit_native=admit_native, native_waiters=set(), waiters={})

    assert await session_ingress.admit_message(authority, SimpleNamespace(source=None)) == 'reply'


@pytest.mark.asyncio
async def test_runtime_draining_rereads_admission_off_the_loop(monkeypatch):
    import asyncio
    import hermes_state_runtime
    from gateway import session_ingress

    loop = asyncio.get_running_loop()
    loop_thread = threading.get_ident()
    settled = threading.Event()
    reads = []

    def read_admission(db, *, admission_id):
        reads.append(threading.get_ident())
        if len(reads) == 1:
            waiter = authority.waiters[admission_id]

            def fail():
                waiter.set_exception(hermes_state_runtime.RuntimeStoreError('runtime_draining'))
                settled.set()

            loop.call_soon_threadsafe(fail)
            assert settled.wait(timeout=2), 'the waiter did not fail during the first status read'
        return {'status': 'queued', 'outcome': None}

    async def admit_native(event):
        return SimpleNamespace(status='queued', admission_id='a3', ref=SimpleNamespace(session_id='s'))

    monkeypatch.setattr(hermes_state_runtime, 'get_session_admission', read_admission)
    authority = SimpleNamespace(
        db=object(), admit_native=admit_native, native_waiters=set(), waiters={},
        sessions={'s': SimpleNamespace(pause_notified=False, source=None)})

    reply = await session_ingress.admit_message(authority, SimpleNamespace(source=None))

    assert reply.startswith('⏳ Hermes is restarting')
    assert len(reads) == 2
    assert all(thread_id != loop_thread for thread_id in reads)


@pytest.mark.asyncio
async def test_stop_owner_turn_reads_the_session_handle_off_the_loop(monkeypatch):
    import hermes_state_runtime
    from gateway.run_inbound import GatewayInboundMixin

    loop_thread = threading.get_ident()
    reads = []

    def handle(ref):
        reads.append(threading.get_ident())
        return SimpleNamespace(execution_state='idle', execution_generation=None)

    monkeypatch.setattr(hermes_state_runtime, 'list_session_admissions', lambda db, *, session_id: [])
    authority = SimpleNamespace(
        profile_id='owned', db=object(), sessions={'s': object()},
        logical_owner=lambda sid: sid, fence_route=lambda key: None, _handle=handle)
    runner = SimpleNamespace(
        session_store=SimpleNamespace(peek_session_id=lambda key: 's'), session_authority=authority)

    await GatewayInboundMixin._hm_stop_owner_turn(runner, 'route-key')
    assert len(reads) == 1
    assert reads[0] != loop_thread, 'the session handle was read on the event loop thread'
