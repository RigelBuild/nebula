"""A delivered exec-approval card keeps its live buttons across a Discord reconnect (RIG-12804).

discord.py keeps a sent view's buttons in its client's view store. ``parse_ready`` clears the
store only with ``views=False``, and ``parse_resumed`` only dispatches the event, so a card
delivered before a RESUME still answers presses. The reconnect path must not re-post or settle
such a card. These tests pin that: neither the first ``on_ready`` nor ``on_resumed`` touches it.
The scan line and the disconnect timestamp feed the backfill's INFO log.
"""

import asyncio
import time
import uuid
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest

from gateway.config import PlatformConfig
from plugins.platforms.discord.adapter import DiscordAdapter
from tests.gateway.test_discord_approval_reconnect import (
    _FakeBot,
    _capture_card,
    _connect,
    _queue_entry,
)
from tests.gateway.test_discord_reconnect_catchup import _SessionStore


async def _deliver_card(adapter, monkeypatch, created, session, request_id):
    """Send a card that Discord confirmed: its view is live and its request still pending."""
    _queue_entry(monkeypatch, session, request_id)
    _capture_card(created[0])
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)
    assert result.success is True
    return adapter._exec_approval_views[request_id]




@pytest.mark.asyncio
async def test_delivered_card_survives_on_resumed(monkeypatch):
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    session = "agent:main:discord:555"
    request_id = uuid.uuid4().hex
    view = await _deliver_card(adapter, monkeypatch, created, session, request_id)

    await created[0]._events["on_resumed"]()
    await asyncio.gather(*list(adapter._background_tasks))

    assert adapter._exec_approval_views[request_id] is view
    assert getattr(view, "undelivered", False) is False
    assert not view.is_finished()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_disconnect_marks_the_start_of_the_gap(monkeypatch):
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    assert adapter._disconnected_at is None

    before = time.monotonic()
    await created[0]._events["on_disconnect"]()

    assert adapter._disconnected_at is not None
    assert before <= adapter._disconnected_at <= time.monotonic()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_backfill_scan_logs_channels_gap_and_counts(monkeypatch, caplog, tmp_path):
    monkeypatch.setenv("HERMES_HOME", str(tmp_path))
    monkeypatch.setenv("DISCORD_MISSED_MESSAGE_BACKFILL", "true")
    monkeypatch.setenv("DISCORD_ALLOW_ALL_USERS", "true")
    adapter = DiscordAdapter(
        PlatformConfig(enabled=True, token="fake-token", extra={"slash_commands": False}))
    adapter._client = SimpleNamespace(user=SimpleNamespace(id=999, bot=True), get_channel=lambda _id: None)
    adapter._handle_message = AsyncMock(return_value=True)
    adapter._session_store = _SessionStore()
    adapter._disconnected_at = time.monotonic() - 42.0

    async def no_candidates(_channels):
        return
        yield  # pragma: no cover - makes this an async generator

    adapter._iter_missed_message_backfill_candidates = no_candidates
    adapter._missed_message_backfill_channels = AsyncMock(return_value={123, 456})

    with caplog.at_level("INFO"):
        await adapter._run_missed_message_backfill()

    lines = [r.getMessage() for r in caplog.records if "Missed-message backfill complete" in r.getMessage()]
    assert len(lines) == 1
    assert "channels=2" in lines[0]
    assert "scanned=0 missed=0 dispatched=0" in lines[0]
    gap = float(lines[0].rsplit("gap_seconds=", 1)[1])
    assert 41.0 <= gap <= 60.0
