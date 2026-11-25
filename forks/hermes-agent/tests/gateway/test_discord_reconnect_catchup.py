"""Discord reconnect catch-up (RIG-12804).

Messages sent while the gateway was disconnected are admitted once each, oldest first, after the
reconnect. The catch-up runs on every connect and on RESUME, scans threads with an active session,
shares the live ingress dedupe, and admits only allowed users.
"""

import asyncio
import threading
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest

from gateway.config import Platform, PlatformConfig
from plugins.platforms.discord import adapter as discord_platform
from tests.gateway.test_discord_missed_message_backfill import FakeChannel, make_message

BOT_ID = 999


class _SessionStore:
    """Stands in for ``SessionStore.list_sessions`` with Discord chat origins."""

    def __init__(self, *sessions):
        self._sessions = []
        for session in sessions:
            chat_id, chat_type = session if isinstance(session, tuple) else (session, "thread")
            self._sessions.append(SimpleNamespace(
                platform=Platform.DISCORD, chat_type=chat_type,
                origin=SimpleNamespace(chat_id=str(chat_id)),
            ))
        self.active_minutes = None

    def list_sessions(self, active_minutes=None):
        self.active_minutes = active_minutes
        return self._sessions


class _FakeBot:
    """A discord.py client stand-in: records gateway event handlers and serves cached channels."""

    def __init__(self, channels, *, intents, allowed_mentions=None, **_):
        self.user = SimpleNamespace(id=BOT_ID, bot=True, display_name="Hermes", name="hermes")
        self.channels = channels
        self.tree = SimpleNamespace(
            sync=AsyncMock(return_value=[]), fetch_commands=AsyncMock(return_value=[]),
            get_commands=lambda *a, **k: [], command=lambda *a, **k: (lambda fn: fn),
        )
        self.http = SimpleNamespace(
            upsert_global_command=AsyncMock(), edit_global_command=AsyncMock(),
            delete_global_command=AsyncMock(),
        )
        self.events = {}
        self._closed = asyncio.Event()

    def event(self, fn):
        self.events[fn.__name__] = fn
        return fn

    def get_channel(self, channel_id):
        return self.channels.get(channel_id)

    async def start(self, token):
        await self.events["on_ready"]()
        await self._closed.wait()

    async def close(self):
        self._closed.set()


@pytest.fixture
def adapter(monkeypatch, tmp_path):
    monkeypatch.setenv("HERMES_HOME", str(tmp_path))
    monkeypatch.setenv("DISCORD_MISSED_MESSAGE_BACKFILL", "true")
    monkeypatch.setenv("DISCORD_ALLOW_ALL_USERS", "true")
    adapter = discord_platform.DiscordAdapter(
        PlatformConfig(enabled=True, token="fake-token", extra={"slash_commands": False}))
    adapter._client = SimpleNamespace(
        user=SimpleNamespace(id=BOT_ID, bot=True), get_channel=lambda _id: None)
    adapter._ready_event.set()
    adapter._handle_message = AsyncMock(return_value=True)
    return adapter


def _mention(message_id, author_id, text, channel):
    """A message that addresses the bot by a literal mention, so the mention gate admits it."""
    return make_message(
        message_id=message_id, author_id=author_id, content=f"<@{BOT_ID}> {text}", channel=channel,
    )


def _admitted(adapter):
    return [call.args[0] for call in adapter._handle_message.await_args_list]


@pytest.mark.asyncio
async def test_gap_in_an_active_session_thread_is_admitted_once_in_order(adapter):
    """Two messages sent during a gap, in a thread with an active session, are admitted once each,
    oldest first. No channel is configured, so only the active session's thread is scanned."""
    thread = FakeChannel(channel_id=456, parent_id=123)
    adapter._client.get_channel = lambda channel_id: thread if channel_id == 456 else None
    adapter._session_store = _SessionStore(456)
    first = _mention(101, 42, "first", thread)
    second = _mention(102, 42, "second", thread)
    thread._history_messages = [second, first]  # newest first, as channel.history() yields them

    await adapter._run_missed_message_backfill()

    assert _admitted(adapter) == [first, second]


@pytest.mark.asyncio
async def test_message_seen_live_and_by_catch_up_is_admitted_once(adapter, monkeypatch):
    """A message that arrives live and is also returned by the catch-up is admitted once."""
    monkeypatch.setenv("DISCORD_MISSED_MESSAGE_BACKFILL_CHANNELS", "123")
    channel = FakeChannel(channel_id=123)
    adapter._client.get_channel = lambda channel_id: channel if channel_id == 123 else None
    earlier = _mention(101, 42, "earlier", channel)
    later = _mention(102, 42, "later", channel)
    channel._history_messages = [later, earlier]

    await adapter._dispatch_discord_message(later)  # live copy of 102 arrives first
    await adapter._run_missed_message_backfill()  # the catch-up finds 101 and 102
    await adapter._dispatch_discord_message(earlier)  # late live copy of 101 after the catch-up

    assert _admitted(adapter) == [later, earlier]


@pytest.mark.asyncio
async def test_session_thread_backfill_admits_only_allowed_users(adapter, monkeypatch):
    """The session-thread source adds a channel without bypassing the Discord user allow-list."""
    monkeypatch.delenv("DISCORD_MISSED_MESSAGE_BACKFILL_CHANNELS", raising=False)
    monkeypatch.setenv("DISCORD_ALLOWED_CHANNELS", "")
    monkeypatch.setenv("DISCORD_FREE_RESPONSE_CHANNELS", "")
    monkeypatch.delenv("DISCORD_ALLOW_ALL_USERS", raising=False)
    adapter._allowed_user_ids = {"42"}
    channel = FakeChannel(channel_id=123)
    adapter._client.get_channel = lambda channel_id: channel if channel_id == 123 else None
    adapter._session_store = _SessionStore(123)
    allowed = _mention(201, 42, "allowed", channel)
    denied = _mention(202, 41, "denied", channel)
    channel._history_messages = [denied, allowed]

    await adapter._run_missed_message_backfill()

    assert _admitted(adapter) == [allowed]

@pytest.mark.asyncio
async def test_session_dm_group_and_thread_chats_are_added_to_explicit_channels(adapter):
    adapter.config.extra["missed_message_backfill"] = {
        "enabled": True, "channels": ["999"], "window_seconds": 172800,
    }
    store = _SessionStore((123, "dm"), (456, "group"), (789, "thread"))
    adapter._session_store = store

    channels = await adapter._missed_message_backfill_channels()

    assert channels == {"123", "456", "789", "999"}
    assert store.active_minutes >= 2880


@pytest.mark.asyncio
async def test_failed_recovered_dispatch_releases_dedupe_for_live_retry(adapter, monkeypatch):
    monkeypatch.setenv("DISCORD_MISSED_MESSAGE_BACKFILL_CHANNELS", "123")
    channel = FakeChannel(channel_id=123)
    adapter._client.get_channel = lambda channel_id: channel if channel_id == 123 else None
    message = _mention(303, 42, "retry me", channel)
    channel._history_messages = [message]
    adapter._handle_message = AsyncMock(side_effect=[RuntimeError("temporary dispatch failure"), True])

    await adapter._run_missed_message_backfill()

    assert adapter._dedup.contains(str(message.id)) is False
    assert await adapter._dispatch_discord_message(message) is True
    assert [call.args[0] for call in adapter._handle_message.await_args_list] == [message, message]


@pytest.mark.asyncio
async def test_backfill_database_operations_run_off_event_loop(adapter, monkeypatch):
    monkeypatch.setenv("DISCORD_MISSED_MESSAGE_BACKFILL_CHANNELS", "123")
    channel = FakeChannel(channel_id=123)
    channel._history_messages = [_mention(404, 42, "check threads", channel)]
    adapter._client.get_channel = lambda channel_id: channel if channel_id == 123 else None
    loop_thread = threading.get_ident()
    called = set()
    names = ("_record_discord_message_seen", "_record_recovery_attempt", "_discord_recovery_cursor",
             "_discord_message_is_persistently_complete", "_discord_message_has_active_claim",
             "_discord_message_attempts_exhausted")
    for name in names:
        original = getattr(adapter, name)

        def checked(*args, _name=name, _original=original, **kwargs):
            assert threading.get_ident() != loop_thread, _name
            called.add(_name)
            return _original(*args, **kwargs)

        monkeypatch.setattr(adapter, name, checked)

    await adapter._run_missed_message_backfill()
    assert called == set(names)


@pytest.mark.asyncio
async def test_resume_runs_the_catch_up_like_a_fresh_connect(adapter, monkeypatch):
    """discord.py RESUMEs in place and fires ``on_resumed`` without ``on_ready``; the catch-up runs then too."""
    monkeypatch.setattr("gateway.status.acquire_scoped_lock", lambda scope, identity, metadata=None: (True, None))
    monkeypatch.setattr("gateway.status_scoped_locks.release_scoped_lock", lambda scope, identity: None)
    monkeypatch.setattr(discord_platform.Intents, "default", lambda: SimpleNamespace(
        message_content=False, dm_messages=False, guild_messages=False, members=False, voice_states=False))
    thread = FakeChannel(channel_id=456, parent_id=123)
    created = []

    def factory(*, command_prefix, intents, allowed_mentions=None, **_):
        bot = _FakeBot({456: thread}, intents=intents, allowed_mentions=allowed_mentions)
        created.append(bot)
        return bot

    monkeypatch.setattr(discord_platform.commands, "Bot", factory)
    monkeypatch.setattr(adapter, "_resolve_allowed_usernames", AsyncMock())
    adapter._session_store = _SessionStore(456)
    adapter._client = None  # a fresh adapter: no prior client to close
    adapter._ready_event.clear()  # connect() must wait for on_ready, as on a real start
    assert await adapter.connect() is True
    await adapter._missed_message_backfill_task  # on_ready's catch-up: nothing sent yet

    first = _mention(101, 42, "first", thread)
    second = _mention(102, 42, "second", thread)
    thread._history_messages = [second, first]  # sent during the resume gap

    await created[0].events["on_resumed"]()
    await adapter._missed_message_backfill_task

    assert _admitted(adapter) == [first, second]
    await adapter.disconnect()
