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



def test_memo_does_not_serve_a_store_written_after_the_snapshot(tmp_path, monkeypatch):
    """A real write (``_save_auth_store``) during the memo window invalidates the snapshot."""
    from hermes_cli import auth

    monkeypatch.setenv("HERMES_HOME", str(tmp_path))
    auth_path = tmp_path / "auth.json"
    auth._save_auth_store({"providers": {"before": {}}}, target_path=auth_path)
    snapshot = auth.prefetch_auth_store(auth_path)
    with auth.auth_store_memo({str(auth_path): snapshot}):
        # No surrounding lock: only the write's own generation bump can invalidate the snapshot.
        auth._save_auth_store({"providers": {"after": {}}}, target_path=auth_path)
        store = auth._load_auth_store(auth_path)
    assert "after" in store["providers"], "the memo served the pre-write store after a write"


def test_memo_does_not_serve_a_store_read_across_an_auth_lock(tmp_path, monkeypatch):
    """Entering and leaving an auth-store lock invalidates the snapshot, as the heal path does."""
    from hermes_cli import auth

    monkeypatch.setenv("HERMES_HOME", str(tmp_path))
    auth_path = tmp_path / "auth.json"
    auth._save_auth_store({"providers": {"before": {}}}, target_path=auth_path)
    snapshot = auth.prefetch_auth_store(auth_path)
    auth_path.write_text('{"version": 1, "providers": {"after": {}}}', encoding="utf-8")
    with auth.auth_store_memo({str(auth_path): snapshot}):
        with auth._auth_store_lock(target_path=auth_path):
            pass
        store = auth._load_auth_store(auth_path)
    assert "after" in store["providers"], "the memo served a pre-lock store after an auth lock"
