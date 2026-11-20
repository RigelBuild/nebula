"""The hygiene settings resolve the session's runtime without reading the auth store on the event loop.

``_hmwa_hygiene_settings`` calls ``_resolve_session_agent_runtime``, which reads ``auth.json`` through
``read_credential_pool`` inline on the loop, on every inbound message (RIG-11272, 16:27 ET block).
"""

import asyncio
import threading

import pytest

from tests.gateway.restart_test_helpers import make_restart_runner, make_restart_source


@pytest.mark.asyncio
async def test_auth_store_read_does_not_block_the_loop_during_hygiene_settings(tmp_path, monkeypatch):
    from hermes_cli import auth

    runner, _adapter = make_restart_runner()
    source = make_restart_source(chat_id="hygiene-auth")
    loop = asyncio.get_running_loop()
    loop_ran = threading.Event()
    observed = []
    real_load = auth._load_auth_store

    def slow_load(*args, **kwargs):
        # The first auth read asks the loop to set loop_ran. Off the loop the wait returns at once;
        # on the loop it times out (the red case).
        if not observed:
            loop.call_soon_threadsafe(loop_ran.set)
            observed.append(loop_ran.wait(timeout=5))
        return real_load(*args, **kwargs)

    monkeypatch.setattr(auth, "_load_auth_store", slow_load)
    await runner._hmwa_hygiene_settings(source, "agent:main:hygiene-auth")
    assert observed == [True], "the auth store read blocked the event loop during hygiene settings"
