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
