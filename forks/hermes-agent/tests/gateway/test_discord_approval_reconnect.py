"""An exec-approval press made after a Discord adapter reconnect must still reach its approval.

Each ``connect()`` builds a new ``commands.Bot``, and discord.py keeps a view's buttons in the
store of the client that sent it. A press on an older client's card is dropped by discord.py
with no reply, so the agent turn never resumes. The adapter answers these presses itself.
"""

import asyncio
import logging
import time
import uuid
from types import SimpleNamespace
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from agent.i18n import t
from gateway.config import PlatformConfig
from plugins.platforms.discord import adapter as discord_platform
from plugins.platforms.discord.adapter import DiscordAdapter
from tools import approval
from tools.approval_gateway_wait import _ApprovalEntry


@pytest.fixture(autouse=True)
def _stubbed_discord_builds_exec_buttons(monkeypatch):
    """``tests/gateway/conftest.py`` stubs discord.py when it is not installed. The stub's ``ui.button`` returns
    the method unchanged and its View builds no items, so ``ExecApprovalView`` would have no children and no
    custom_ids. Give it what discord.py gives a real view: a Button item per decorated button, set on the
    instance and listed in ``children``, plus ``remove_item``. A no-op when the real library is installed."""
    if hasattr(discord_platform.discord, "__file__"):
        return
    view_base = discord_platform.discord.ui.View
    base_init = view_base.__init__

    def init(self, *args, **kwargs):
        base_init(self, *args, **kwargs)
        if type(self).__name__ != "ExecApprovalView":
            return
        for name in ("allow_once", "allow_session", "allow_always", "deny"):
            item = discord_platform.discord.ui.Button(label=name)
            item.callback = getattr(self, name)
            setattr(self, name, item)
            self.add_item(item)

    def remove_item(self, item):
        self.children[:] = [child for child in self.children if child is not item]

    monkeypatch.setattr(view_base, "__init__", init)
    monkeypatch.setattr(view_base, "remove_item", remove_item, raising=False)


class _FakeBot:
    def __init__(self, *, intents, allowed_mentions=None, **_):
        self.intents = intents
        self.allowed_mentions = allowed_mentions
        self.application_id = 999
        self.user = SimpleNamespace(id=999, name="Hermes")
        self._events = {}
        self._closed = asyncio.Event()
        self.tree = SimpleNamespace(
            sync=AsyncMock(return_value=[]),
            fetch_commands=AsyncMock(return_value=[]),
            get_commands=lambda *a, **k: [],
            command=lambda *a, **k: (lambda fn: fn),
        )
        self.http = SimpleNamespace(
            upsert_global_command=AsyncMock(), edit_global_command=AsyncMock(),
            delete_global_command=AsyncMock(),
        )

    def event(self, fn):
        self._events[fn.__name__] = fn
        return fn

    def get_channel(self, _channel_id):
        return None  # a real client has no cached channels until READY

    def get_partial_messageable(self, _channel_id, **_kw):
        raise AssertionError("set per test")

    async def start(self, token):
        # A live websocket runs until closed. Returning early would make the adapter's gateway task exit and
        # be treated as a post-startup fatal, which re-queues the reconnected adapter.
        if "on_ready" in self._events:
            await self._events["on_ready"]()
        await self._closed.wait()

    async def close(self):
        self._closed.set()



async def _connect(adapter, monkeypatch, created):
    monkeypatch.setattr("gateway.status.acquire_scoped_lock", lambda scope, identity, metadata=None: (True, None))
    monkeypatch.setattr("gateway.status_scoped_locks.release_scoped_lock", lambda scope, identity: None)
    monkeypatch.setattr(discord_platform.Intents, "default", lambda: SimpleNamespace(
        message_content=False, dm_messages=False, guild_messages=False, members=False, voice_states=False))

    def factory(*, command_prefix, intents, allowed_mentions=None, **_):
        bot = _FakeBot(intents=intents, allowed_mentions=allowed_mentions)
        created.append(bot)
        return bot

    monkeypatch.setattr(discord_platform.commands, "Bot", factory)
    monkeypatch.setattr(adapter, "_resolve_allowed_usernames", AsyncMock())
    assert await adapter.connect() is True


def _capture_card(bot, message_id=1234):
    """Make ``bot`` send the card into a channel and record its kwargs (view, embed, message)."""
    card = {}

    async def send(**kwargs):
        message = SimpleNamespace(id=message_id, edit=AsyncMock(), embeds=[kwargs["embed"]])
        card.update(kwargs)
        card["message"] = message
        return message

    channel = SimpleNamespace(send=AsyncMock(side_effect=send))
    bot.get_channel = lambda _chat_id: channel
    return card


def _queue_entry(monkeypatch, session, request_id):
    """Put one pending gateway approval for ``request_id`` on ``session``'s queue."""
    entry = _ApprovalEntry({"request_id": request_id, "command": "rm x"})
    entry.authority = {"requester_id": "11111"}
    monkeypatch.setitem(approval._gateway_queues, session, [entry])
    return entry


def _press(custom_id, *, message=None, user_id="11111"):
    """A component press. ``is_done()`` turns True once the interaction is answered (a first response or a
    ``defer()``), as discord.py does. ``message.edit`` is the post-defer card edit; ``followup.send`` the reply."""
    answered = {"done": False}

    def _mark_answered(*_args, **_kwargs):
        answered["done"] = True

    response = SimpleNamespace(
        send_message=AsyncMock(side_effect=_mark_answered),
        edit_message=AsyncMock(side_effect=_mark_answered),
        defer=AsyncMock(side_effect=_mark_answered),
        is_done=lambda: answered["done"])
    followup = SimpleNamespace(send=AsyncMock())
    interaction = SimpleNamespace(
        data={"custom_id": custom_id},
        user=SimpleNamespace(id=user_id, display_name="alice"),
        response=response,
        followup=followup,
        message=message,
        channel_id=555,
        guild=None,
        channel=None,
    )
    return interaction, response


@pytest.mark.asyncio
async def test_pending_approval_press_after_reconnect_resolves_request(monkeypatch):
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _ApprovalEntry({"request_id": request_id, "command": "rm x"})
    entry.authority = {"requester_id": "11111"}
    monkeypatch.setitem(approval._gateway_queues, session, [entry])

    card = _capture_card(created[0])
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)
    assert result.success is True

    # The card was sent on the first client; a reconnect replaces it.
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}
    assert len(created) == 2

    on_interaction = created[1]._events["on_interaction"]
    interaction, response = _press(f"hermes:exec:{request_id}:once", message=card["message"])
    await on_interaction(interaction)

    assert entry.result == "once"
    assert entry.event.is_set()
    response.defer.assert_awaited_once()
    card["message"].edit.assert_awaited_once()
    response.edit_message.assert_not_awaited()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_approve_on_live_client_after_resolve_logs_no_unmatched_warning(monkeypatch, caplog):
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}
    caplog.set_level(logging.INFO)

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _ApprovalEntry({"request_id": request_id, "command": "rm x"})
    entry.authority = {"requester_id": "11111"}
    monkeypatch.setitem(approval._gateway_queues, session, [entry])

    card = _capture_card(created[0])
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)
    assert result.success is True

    # The live Approve resolves the entry and finishes the card's view (``_finalize_embed`` stops it).
    custom_id = f"hermes:exec:{request_id}:once"
    interaction, response = _press(custom_id, message=card["message"])
    await card["view"].press(interaction, custom_id)
    assert entry.result == "once"
    assert card["view"].is_finished()
    response.defer.assert_awaited_once()
    card["message"].edit.assert_awaited_once()
    response.edit_message.assert_not_awaited()
    caplog.clear()

    # discord.py hands the same answered interaction to the live client's on_interaction.
    on_interaction = created[0]._events["on_interaction"]
    await on_interaction(interaction)

    response.send_message.assert_not_awaited()
    response.defer.assert_awaited_once()
    assert card["message"].edit.await_count == 1
    assert not any(r.levelno == logging.WARNING for r in caplog.records)
    assert any(r.levelno == logging.INFO and "approval press already answered" in r.getMessage()
               and f"custom_id={custom_id}" in r.getMessage() for r in caplog.records)
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_unmatched_press_gets_ephemeral_reply_and_log(monkeypatch, caplog):
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    caplog.set_level(logging.INFO)
    on_interaction = created[0]._events["on_interaction"]

    interaction, response = _press(f"hermes:exec:{uuid.uuid4().hex}:once")
    await on_interaction(interaction)

    response.send_message.assert_awaited_once()
    assert response.send_message.await_args.args[0] == t("platform.discord.approval.no_longer_pending")
    assert response.send_message.await_args.kwargs.get("ephemeral") is True
    warnings = [r for r in caplog.records if r.levelno == logging.WARNING]
    assert any("custom_id=hermes:exec:" in r.getMessage() for r in warnings)

    caplog.set_level(logging.DEBUG)
    caplog.clear()
    other, other_response = _press("hermes:model:abc")
    await on_interaction(other)

    other_response.send_message.assert_not_awaited()
    assert any(r.levelno == logging.DEBUG and "custom_id=hermes:model:abc" in r.getMessage()
               for r in caplog.records)
    assert not any("custom_id=hermes:model:abc" in r.getMessage()
                   and r.levelno >= logging.INFO for r in caplog.records)
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_live_finished_view_unanswered_press_gets_no_longer_pending(monkeypatch, caplog):
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}
    caplog.set_level(logging.INFO)

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    card = _capture_card(created[0])
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)
    assert result.success is True

    # The card timed out on the live client. The timeout withdraws the request, so no pending entry is left.
    await card["view"].on_timeout()
    assert card["view"].is_finished()
    caplog.clear()

    custom_id = f"hermes:exec:{request_id}:once"
    on_interaction = created[0]._events["on_interaction"]
    interaction, response = _press(custom_id, message=card["message"])
    await on_interaction(interaction)

    response.send_message.assert_awaited_once()
    assert response.send_message.await_args.args[0] == t("platform.discord.approval.no_longer_pending")
    assert response.send_message.await_args.kwargs.get("ephemeral") is True
    assert any(r.levelno == logging.WARNING and f"custom_id={custom_id}" in r.getMessage()
               for r in caplog.records)
    assert entry.result is None
    assert entry.cancelled
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_card_buttons_carry_stable_ids_for_kept_choices_only(monkeypatch):
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}

    card = _capture_card(created[0])
    full_id = uuid.uuid4().hex
    full_session = "agent:full:discord:555"
    _queue_entry(monkeypatch, full_session, full_id)
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=full_session, request_id=full_id)
    assert result.success is True
    assert {c.custom_id for c in card["view"].children} == {
        f"hermes:exec:{full_id}:{choice}" for choice in ("once", "session", "always", "deny")}

    no_always_id = uuid.uuid4().hex
    no_always_session = "agent:noalways:discord:555"
    no_always_entry = _queue_entry(monkeypatch, no_always_session, no_always_id)
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=no_always_session, request_id=no_always_id,
        allow_permanent=False)
    assert result.success is True
    view = card["view"]
    assert {c.custom_id for c in view.children} == {
        f"hermes:exec:{no_always_id}:{choice}" for choice in ("once", "session", "deny")}

    always_id = f"hermes:exec:{no_always_id}:always"
    interaction, response = _press(always_id, message=card["message"])
    assert await view.press(interaction, always_id) is False
    assert no_always_entry.result is None
    assert not no_always_entry.event.is_set()
    response.send_message.assert_not_awaited()
    response.edit_message.assert_not_awaited()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_orphan_press_by_unlisted_user_is_refused_and_entry_stays_pending(monkeypatch):
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    card = _capture_card(created[0])
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)
    assert result.success is True

    # Reconnect: the card's view is now an orphan, so the adapter answers its presses.
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}
    assert len(created) == 2

    on_interaction = created[1]._events["on_interaction"]
    interaction, response = _press(
        f"hermes:exec:{request_id}:once", message=card["message"], user_id="99999")
    await on_interaction(interaction)

    assert entry.result is None
    assert not entry.event.is_set()
    response.send_message.assert_awaited_once()
    assert response.send_message.await_args.args[0] == discord_platform._unauthorized()
    assert response.send_message.await_args.kwargs.get("ephemeral") is True
    response.edit_message.assert_not_awaited()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_orphan_press_of_removed_button_logs_warning_and_gets_no_longer_pending(monkeypatch, caplog):
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}
    caplog.set_level(logging.INFO)

    session = "agent:noalways:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    card = _capture_card(created[0])
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id, allow_permanent=False)
    assert result.success is True

    # Reconnect: the card is now an orphan, and it has no "always" button.
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}
    caplog.clear()

    custom_id = f"hermes:exec:{request_id}:always"
    on_interaction = created[1]._events["on_interaction"]
    interaction, response = _press(custom_id, message=card["message"])
    await on_interaction(interaction)

    response.send_message.assert_awaited_once()
    assert response.send_message.await_args.args[0] == t("platform.discord.approval.no_longer_pending")
    assert response.send_message.await_args.kwargs.get("ephemeral") is True
    response.edit_message.assert_not_awaited()
    assert entry.result is None
    assert not entry.event.is_set()
    assert any(r.levelno == logging.WARNING and "orphaned approval press matched no live button" in r.getMessage()
               and f"custom_id={custom_id}" in r.getMessage() for r in caplog.records)
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_orphan_press_that_raises_gets_click_failed_and_warning(monkeypatch, caplog):
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}
    caplog.set_level(logging.INFO)

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    card = _capture_card(created[0])
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)
    assert result.success is True

    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}
    caplog.clear()

    async def boom(_interaction, _custom_id):
        raise RuntimeError("press failed")

    monkeypatch.setattr(card["view"], "press", boom)
    custom_id = f"hermes:exec:{request_id}:once"
    on_interaction = created[1]._events["on_interaction"]
    interaction, response = _press(custom_id, message=card["message"])
    await on_interaction(interaction)

    response.send_message.assert_awaited_once()
    assert response.send_message.await_args.args[0] == t("platform.discord.approval.click_failed")
    assert response.send_message.await_args.kwargs.get("ephemeral") is True
    response.edit_message.assert_not_awaited()
    assert entry.result is None
    assert any(r.levelno == logging.WARNING and "orphaned approval press failed" in r.getMessage()
               and f"custom_id={custom_id}" in r.getMessage() for r in caplog.records)
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_orphan_press_whose_edit_fails_after_recording_answer_gets_already_resolved(monkeypatch):
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    card = _capture_card(created[0])
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)
    assert result.success is True

    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}

    # The real view.press runs. The answer is recorded, then the card edit fails without answering the
    # interaction, so the adapter's orphan path must still reply.
    custom_id = f"hermes:exec:{request_id}:once"
    on_interaction = created[1]._events["on_interaction"]
    interaction, response = _press(custom_id, message=card["message"])
    # The post-defer card edit fails after the answer is recorded; retire() then stamps the card through its message.
    card["message"].edit = AsyncMock(side_effect=[RuntimeError("edit failed after answer"), None])
    await on_interaction(interaction)

    assert entry.result == "once"
    response.defer.assert_awaited_once()
    assert card["message"].edit.await_count == 2
    assert card["message"].edit.await_args.kwargs["view"] is None
    interaction.followup.send.assert_awaited_once()
    assert interaction.followup.send.await_args.args[0] == t("platform.discord.approval.already_resolved")
    assert interaction.followup.send.await_args.kwargs.get("ephemeral") is True
    response.send_message.assert_not_awaited()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_card_sent_across_reconnect_is_resolved_by_the_new_client(monkeypatch):
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    card = {}

    async def send(**kwargs):
        # The reconnect lands while the card is still in flight on the old client.
        await _connect(adapter, monkeypatch, created)
        adapter._allowed_user_ids = {"11111"}
        message = SimpleNamespace(id=1234, edit=AsyncMock(), embeds=[kwargs["embed"]])
        card.update(kwargs)
        card["message"] = message
        return message

    channel = SimpleNamespace(send=AsyncMock(side_effect=send))
    created[0].get_channel = lambda _chat_id: channel
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)
    assert result.success is True
    assert len(created) == 2

    custom_id = f"hermes:exec:{request_id}:once"
    interaction, response = _press(custom_id, message=card["message"])
    await created[1]._events["on_interaction"](interaction)

    assert entry.result == "once"
    assert entry.event.is_set()
    response.defer.assert_awaited_once()
    card["message"].edit.assert_awaited_once()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_orphan_press_naming_another_cards_message_is_refused(monkeypatch, caplog):
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}
    caplog.set_level(logging.INFO)

    session_a = "agent:a:discord:555"
    session_b = "agent:b:discord:555"
    request_a = uuid.uuid4().hex
    request_b = uuid.uuid4().hex
    entry_a = _queue_entry(monkeypatch, session_a, request_a)
    _queue_entry(monkeypatch, session_b, request_b)

    _card_a = _capture_card(created[0], message_id=1111)
    assert (await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session_a, request_id=request_a)).success is True
    card_b = _capture_card(created[0], message_id=2222)
    assert (await adapter.send_exec_approval(
        chat_id="555", command="rm y", session_key=session_b, request_id=request_b)).success is True

    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}
    caplog.clear()

    # Card A's button, pressed on card B's message.
    custom_id = f"hermes:exec:{request_a}:once"
    on_interaction = created[1]._events["on_interaction"]
    interaction, response = _press(custom_id, message=card_b["message"])
    await on_interaction(interaction)

    assert entry_a.result is None
    assert not entry_a.event.is_set()
    response.send_message.assert_awaited_once()
    assert response.send_message.await_args.args[0] == t("platform.discord.approval.no_longer_pending")
    assert response.send_message.await_args.kwargs.get("ephemeral") is True
    response.edit_message.assert_not_awaited()
    assert any(r.levelno == logging.WARNING and "component press matched no pending approval" in r.getMessage()
               and f"custom_id={custom_id}" in r.getMessage() for r in caplog.records)
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_live_unfinished_view_press_is_left_to_discord(monkeypatch):
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    card = _capture_card(created[0])
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)
    assert result.success is True
    assert not card["view"].is_finished()

    # The live client's store routes this press to the view, so the listener must not answer it.
    custom_id = f"hermes:exec:{request_id}:once"
    interaction, response = _press(custom_id, message=card["message"])
    await created[0]._events["on_interaction"](interaction)

    response.send_message.assert_not_awaited()
    response.edit_message.assert_not_awaited()
    assert entry.result is None
    assert not entry.event.is_set()
    await adapter.disconnect()


class _StubApprovalView:
    def __init__(self):
        self.done = False

    def is_finished(self):
        return self.done


def test_exec_approval_view_registry_prunes_finished_then_oldest_past_cap():
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    cap = adapter._EXEC_APPROVAL_VIEW_CAP
    views: dict[str, _StubApprovalView] = {}

    def register(rid):
        views[rid] = _StubApprovalView()
        adapter._register_exec_approval_view(rid, views[rid])
        assert len(adapter._exec_approval_views) <= cap

    for i in range(cap):
        register(f"s{i}")
    settled = [f"s{i}" for i in range(10, 20)]
    for rid in settled:
        views[rid].done = True

    # Over the cap with settled views present: the settled ones are dropped first, so no unfinished
    # view is evicted yet.
    register("n0")
    assert not set(settled) & set(adapter._exec_approval_views)
    assert "s0" in adapter._exec_approval_views
    assert len(adapter._exec_approval_views) == cap - len(settled) + 1

    # With no settled views left, the oldest unfinished views go first.
    for i in range(1, 40):
        register(f"n{i}")
    assert len(adapter._exec_approval_views) == cap
    assert "s0" not in adapter._exec_approval_views
    assert "n39" in adapter._exec_approval_views


@pytest.mark.asyncio
async def test_failed_card_send_is_kept_as_undelivered(monkeypatch):
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    _queue_entry(monkeypatch, session, request_id)

    channel = SimpleNamespace(send=AsyncMock(side_effect=RuntimeError("send failed")))
    created[0].get_channel = lambda _chat_id: channel
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)

    assert result.success is False
    # Kept, marked undelivered: the reconnect re-post and the text-answer gate find a failed card by request id.
    assert adapter._exec_approval_views[request_id].undelivered is True
    assert adapter._exec_approval_views[request_id]._message is None
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_unconfirmed_card_is_not_marked_never_shown_when_the_text_fallback_also_fails(monkeypatch):
    """A card that is not confirmed in time may already be on screen, so the prompt is not 'never shown':
    a failed text fallback must not mark the request undelivered. The timed-out request is still withdrawn."""
    from gateway.platforms.base import ExecApprovalPrompt

    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                            extra={"approval_card_confirm_timeout": 0.2}))
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    async def card_never_confirmed(_prompt, _view):
        await asyncio.Event().wait()
    monkeypatch.setattr(adapter, "_send_exec_approval_card", card_never_confirmed)
    monkeypatch.setattr(adapter, "_send_exec_approval_text_fallback", AsyncMock(return_value="send_path_degraded"))
    prompt = ExecApprovalPrompt(
        chat_id="555", session_key=session, text="rm x", command="rm x", smart_denied=False,
        actions=[("Allow once", "once", "primary"), ("Deny", "deny", "danger")],
        request_id=request_id, metadata={"thread_id": "777"})

    result = await adapter._send_exec_approval_prompt(prompt)

    assert result.error == discord_platform._CARD_CONFIRM_TIMEOUT_ERROR
    assert entry.undelivered is None
    assert await _until(lambda: entry not in approval._gateway_queues.get(session, []), timeout=3)
    assert entry.cancelled == "the approval card was not delivered"


@pytest.mark.asyncio
async def test_failed_card_with_failed_text_fallback_is_marked_never_shown(monkeypatch):
    """Counterpart: a card that failed outright (not a confirm timeout) and a failed text fallback leave no
    surface showing the prompt, so the request is marked undelivered with both failure reasons."""
    from gateway.platforms.base import ExecApprovalPrompt, SendResult

    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                            extra={"approval_card_confirm_timeout": 0.2}))
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    monkeypatch.setattr(adapter, "_send_exec_approval_card",
                        AsyncMock(return_value=SendResult(success=False, error="Not connected")))
    monkeypatch.setattr(adapter, "_send_exec_approval_text_fallback", AsyncMock(return_value="send_path_degraded"))
    prompt = ExecApprovalPrompt(
        chat_id="555", session_key=session, text="rm x", command="rm x", smart_denied=False,
        actions=[("Allow once", "once", "primary"), ("Deny", "deny", "danger")],
        request_id=request_id, metadata={"thread_id": "777"})

    await adapter._send_exec_approval_prompt(prompt)

    assert entry.undelivered == "Not connected, then send_path_degraded"
    assert await _until(lambda: entry not in approval._gateway_queues.get(session, []), timeout=3)

@pytest.mark.asyncio
async def test_ui_timeout_then_reconnect_press_ends_wait_as_cancelled(monkeypatch):
    # Live sequence: the card's UI timeout finishes the view; a reconnect swaps the client; an
    # Approve then arrives. The timeout withdraws the request at once, so the wait ends cancelled and the
    # press gets "no longer pending" instead of hanging the agent turn until /stop.
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    card = _capture_card(created[0])
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)
    assert result.success is True

    await card["view"].on_timeout()
    assert card["view"].is_finished()
    assert entry.event.is_set()
    assert entry.cancelled
    assert entry.result is None

    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}

    custom_id = f"hermes:exec:{request_id}:once"
    on_interaction = created[1]._events["on_interaction"]
    interaction, response = _press(custom_id, message=card["message"])
    await on_interaction(interaction)

    assert entry.result is None
    response.send_message.assert_awaited_once()
    assert response.send_message.await_args.args[0] == t("platform.discord.approval.no_longer_pending")
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_lost_view_press_settles_pending_request_as_cancelled(monkeypatch):
    # A pending request whose card view is gone (lost from the registry) must not hang the agent turn: the
    # adapter cannot answer it, so the wait ends at once as withdrawn, never as a user deny.
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    card = _capture_card(created[0])
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)
    assert result.success is True

    # The view is lost from the registry while the request is still pending, then Approve arrives.
    adapter._exec_approval_views.pop(request_id)
    custom_id = f"hermes:exec:{request_id}:once"
    on_interaction = created[0]._events["on_interaction"]
    interaction, response = _press(custom_id, message=card["message"], user_id="11111")
    await on_interaction(interaction)

    assert entry.event.is_set()
    assert entry.cancelled
    assert entry.result is None
    response.send_message.assert_awaited_once()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_unauthorized_lost_view_press_leaves_the_request_pending(monkeypatch):
    # A press on a card whose view is gone must not withdraw the request for a clicker the adapter refuses:
    # the same gates as a live card apply first, so an outsider cannot end someone else's approval.
    for name in ("DISCORD_ALLOW_ALL_USERS", "GATEWAY_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS"):
        monkeypatch.delenv(name, raising=False)
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    card = _capture_card(created[0])
    assert (await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)).success is True

    adapter._exec_approval_views.pop(request_id)
    custom_id = f"hermes:exec:{request_id}:once"
    on_interaction = created[0]._events["on_interaction"]
    interaction, response = _press(custom_id, message=card["message"], user_id="99999")
    await on_interaction(interaction)

    assert not entry.event.is_set()
    assert entry.result is None
    assert entry.cancelled is None
    assert request_id in [e.data.get("request_id") for e in approval._gateway_queues.get(session, [])]
    response.send_message.assert_awaited_once()
    assert response.send_message.await_args.args[0] == discord_platform._unauthorized()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_lost_view_press_by_non_requester_gets_the_refusal_and_stays_pending(monkeypatch):
    # The view is truly lost (popped, not retired). An admitted clicker who did not start the turn gets the requester
    # refusal, and the request stays pending: the lost-view branch applies the same authority rule as a live card.
    from gateway.approval_actor import refusal_text
    for name in ("DISCORD_ALLOW_ALL_USERS", "GATEWAY_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS"):
        monkeypatch.delenv(name, raising=False)
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111", "33333"}

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    entry.authority = {"requester_id": "11111", "requester_name": "alice"}

    card = _capture_card(created[0])
    assert (await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)).success is True

    adapter._exec_approval_views.pop(request_id)
    custom_id = f"hermes:exec:{request_id}:once"
    on_interaction = created[0]._events["on_interaction"]
    interaction, response = _press(custom_id, message=card["message"], user_id="33333")
    await on_interaction(interaction)

    assert entry.result is None
    assert not entry.event.is_set()
    assert entry.cancelled is None
    assert request_id in [e.data.get("request_id") for e in approval._gateway_queues.get(session, [])]
    response.send_message.assert_awaited_once()
    assert response.send_message.await_args.args[0] == refusal_text("alice")
    assert response.send_message.await_args.kwargs.get("ephemeral") is True
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_lost_view_press_by_non_admin_is_refused_when_require_admin_is_on(monkeypatch):
    # The lost-view branch must apply require_admin, as a live card does: an admitted clicker who is not an admin
    # is refused and the request stays pending, even though the requester would otherwise be allowed.
    for name in ("DISCORD_ALLOW_ALL_USERS", "GATEWAY_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS"):
        monkeypatch.delenv(name, raising=False)
    extra = {"require_admin_for_exec_approval": True, "allow_admin_from": ["44444"]}
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token", extra=extra))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111", "33333"}

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    entry.authority = {"requester_id": "11111", "requester_name": "alice"}

    card = _capture_card(created[0])
    assert (await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)).success is True

    adapter._exec_approval_views.pop(request_id)
    custom_id = f"hermes:exec:{request_id}:once"
    on_interaction = created[0]._events["on_interaction"]
    interaction, response = _press(custom_id, message=card["message"], user_id="11111")
    await on_interaction(interaction)

    assert entry.result is None
    assert not entry.event.is_set()
    assert entry.cancelled is None
    assert request_id in [e.data.get("request_id") for e in approval._gateway_queues.get(session, [])]
    response.send_message.assert_awaited_once()
    assert response.send_message.await_args.args[0] == discord_platform._unauthorized()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_unauthorized_unmatched_press_on_pending_card_resolves_nothing(monkeypatch):
    # Authority: a press that reaches the unmatched fallback must not settle a pending request for a user
    # the card's own gates refuse. Only the one-click requester (or an admin) may answer it.
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111", "99999"}

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    card = _capture_card(created[0])
    assert (await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)).success is True
    # Finished by a settle that left the request pending: the press is unmatched, so the fallback runs.
    await card["view"].retire("resolved")
    entry.result = None
    monkeypatch.setitem(approval._gateway_queues, session, [entry])

    custom_id = f"hermes:exec:{request_id}:once"
    on_interaction = created[0]._events["on_interaction"]
    interaction, response = _press(custom_id, message=card["message"], user_id="99999")
    await on_interaction(interaction)

    assert entry.result is None
    assert not entry.event.is_set()
    response.send_message.assert_awaited_once()
    assert response.send_message.await_args.kwargs.get("ephemeral") is True
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_non_requester_unmatched_press_gets_the_refusal_reply(monkeypatch):
    # A clicker the adapter admits but who did not start the turn gets the requester refusal, and the request stays pending.
    from gateway.approval_actor import refusal_text
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111", "33333"}

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    entry.authority = {"requester_id": "11111", "requester_name": "alice"}
    card = _capture_card(created[0])
    assert (await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)).success is True
    await card["view"].retire("resolved")
    entry.result = None
    monkeypatch.setitem(approval._gateway_queues, session, [entry])

    custom_id = f"hermes:exec:{request_id}:once"
    on_interaction = created[0]._events["on_interaction"]
    interaction, response = _press(custom_id, message=card["message"], user_id="33333")
    await on_interaction(interaction)

    assert entry.result is None
    assert not entry.event.is_set()
    response.send_message.assert_awaited_once()
    assert response.send_message.await_args.args[0] == refusal_text("alice")
    assert response.send_message.await_args.kwargs.get("ephemeral") is True
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_authorized_unmatched_press_on_pending_card_resolves_request(monkeypatch):
    # The positive control for the refusal above: the same path resolves when the clicker is the requester.
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    card = _capture_card(created[0])
    assert (await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)).success is True
    await card["view"].retire("resolved")
    entry.result = None
    monkeypatch.setitem(approval._gateway_queues, session, [entry])

    custom_id = f"hermes:exec:{request_id}:once"
    on_interaction = created[0]._events["on_interaction"]
    interaction, _response = _press(custom_id, message=card["message"], user_id="11111")
    await on_interaction(interaction)

    assert entry.result == "once"
    assert entry.event.is_set()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_press_after_runner_rebuilds_discord_adapter_resolves_with_pressed_choice(monkeypatch):
    # The incident path: adapter A sends the card, then fails with the stale-websocket fatal
    # (retryable). The runner's _reconnect_failed_platform builds a NEW adapter B. A press on B must end the
    # pending wait with the pressed choice, not hang until /stop.
    from gateway.config import Platform
    from tests.gateway.test_platform_reconnect import _make_runner
    created: list[_FakeBot] = []
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    old = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    await _connect(old, monkeypatch, created)
    old._allowed_user_ids = {"11111"}
    card = _capture_card(created[0])
    assert (await old.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)).success is True

    runner = _make_runner()
    runner.config.platforms[Platform.DISCORD] = PlatformConfig(enabled=True, token="test-token")
    runner.stop = AsyncMock()
    runner._sync_voice_mode_state_to_adapter = MagicMock()
    runner.adapters[Platform.DISCORD] = old
    old._set_fatal_error("discord_websocket_health_stale", "websocket stalled", retryable=True)
    await runner._handle_adapter_fatal_error(old)

    new = DiscordAdapter(PlatformConfig(enabled=True, token="test-token", extra={"allow_from": ["11111"]}))
    await _connect(new, monkeypatch, created)
    with patch.object(runner, "_create_adapter", return_value=new):
        await runner._reconnect_failed_platform(Platform.DISCORD, time.monotonic() + 1)
    assert runner.adapters[Platform.DISCORD] is new

    custom_id = f"hermes:exec:{request_id}:once"
    interaction, _response = _press(custom_id, message=card["message"])
    await created[-1]._events["on_interaction"](interaction)

    assert entry.result == "once"
    assert entry.event.is_set()
    await new.disconnect()


@pytest.mark.asyncio
async def test_rebuild_withdraws_pending_request_whose_card_view_is_finished(monkeypatch):
    # The replacement cannot answer a card whose view already finished. The request must settle at
    # once as withdrawn (not a user deny), so the agent turn does not wait for /stop.
    created: list[_FakeBot] = []
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    old = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    await _connect(old, monkeypatch, created)
    old._allowed_user_ids = {"11111"}
    card = _capture_card(created[0])
    assert (await old.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)).success is True
    await card["view"].retire("resolved")
    assert card["view"].is_finished()

    new = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    new.adopt_exec_approval_views(old)

    assert entry.event.is_set()
    assert entry.cancelled == "the approval card was lost in a platform reconnect"
    assert entry.result is None
    assert request_id not in new._exec_approval_views
    await old.disconnect()


@pytest.mark.asyncio
async def test_authorized_unmatched_press_is_acknowledged_not_left_as_interaction_failed(monkeypatch):
    # When the fallback resolves, the clicker's interaction must be answered. Discord shows
    # "This interaction failed" for an unanswered press, and a second press would then read "no longer pending".
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    card = _capture_card(created[0])
    assert (await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)).success is True
    await card["view"].retire("resolved")
    entry.result = None
    monkeypatch.setitem(approval._gateway_queues, session, [entry])

    custom_id = f"hermes:exec:{request_id}:once"
    on_interaction = created[0]._events["on_interaction"]
    interaction, response = _press(custom_id, message=card["message"], user_id="11111")
    await on_interaction(interaction)

    assert entry.result == "once"
    response.send_message.assert_awaited_once()
    assert response.send_message.await_args.kwargs.get("ephemeral") is True
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_unlisted_unmatched_press_is_refused_even_by_the_requester(monkeypatch):
    # Base admission: the requester passes the queue's authority check, so only the card's own
    # _check_auth can refuse a clicker who is not on the platform allowlist. Removing that gate must fail this test.
    for name in ("DISCORD_ALLOW_ALL_USERS", "GATEWAY_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS"):
        monkeypatch.delenv(name, raising=False)
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    card = _capture_card(created[0])
    assert (await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)).success is True
    await card["view"].retire("resolved")
    entry.result = None
    monkeypatch.setitem(approval._gateway_queues, session, [entry])

    # 22222 is the requester in the queue but not on the platform allowlist.
    entry.authority = {"requester_id": "22222"}
    custom_id = f"hermes:exec:{request_id}:once"
    on_interaction = created[0]._events["on_interaction"]
    interaction, response = _press(custom_id, message=card["message"], user_id="22222")
    await on_interaction(interaction)

    assert entry.result is None
    assert not entry.event.is_set()
    response.send_message.assert_awaited_once()
    assert response.send_message.await_args.args[0] == discord_platform._unauthorized()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_non_admin_unmatched_press_is_refused_when_require_admin_is_on(monkeypatch):
    # require_admin: the clicker passes base admission and the queue's authority (requester), but is not
    # one of the allow_admin_from ids. Removing the view's require_admin gate must fail this test.
    for name in ("DISCORD_ALLOW_ALL_USERS", "GATEWAY_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS"):
        monkeypatch.delenv(name, raising=False)
    adapter = DiscordAdapter(PlatformConfig(
        enabled=True, token="test-token",
        extra={"require_admin_for_exec_approval": True, "allow_admin_from": ["99999"]}))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    card = _capture_card(created[0])
    assert (await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)).success is True
    await card["view"].retire("resolved")
    entry.result = None
    monkeypatch.setitem(approval._gateway_queues, session, [entry])

    # 11111 is the requester and on the allowlist, but is not in allow_admin_from.
    custom_id = f"hermes:exec:{request_id}:once"
    on_interaction = created[0]._events["on_interaction"]
    interaction, _response = _press(custom_id, message=card["message"], user_id="11111")
    await on_interaction(interaction)

    assert entry.result is None
    assert not entry.event.is_set()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_approve_on_deny_only_request_resolves_nothing_but_deny_does(monkeypatch):
    # A Deny-only request (its card could not show the whole command) must refuse an approve press
    # from the fallback. The Deny press still resolves it. Removing the Deny-only check must fail the first assert.
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    card = _capture_card(created[0])
    assert (await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)).success is True
    await card["view"].retire("resolved")
    entry.result = None
    entry.card_deny_only = True
    monkeypatch.setitem(approval._gateway_queues, session, [entry])

    on_interaction = created[0]._events["on_interaction"]
    interaction, _response = _press(f"hermes:exec:{request_id}:once", message=card["message"], user_id="11111")
    await on_interaction(interaction)
    assert entry.result is None
    assert not entry.event.is_set()

    interaction, _response = _press(f"hermes:exec:{request_id}:deny", message=card["message"], user_id="11111")
    await on_interaction(interaction)
    assert entry.result == "deny"
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_unmatched_press_for_a_choice_the_card_lacks_resolves_nothing(monkeypatch):
    # The fallback must check the choice is one of the card's own enabled buttons, as press() does. This
    # card has no "always" button (allow_permanent=False), so an always press is forged and must resolve nothing.
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    card = _capture_card(created[0])
    assert (await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id,
        allow_permanent=False)).success is True
    await card["view"].retire("resolved")
    entry.result = None
    monkeypatch.setitem(approval._gateway_queues, session, [entry])

    on_interaction = created[0]._events["on_interaction"]
    interaction, _response = _press(f"hermes:exec:{request_id}:always", message=card["message"], user_id="11111")
    await on_interaction(interaction)

    assert entry.result is None
    assert not entry.event.is_set()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_press_after_secondary_rebuilds_discord_adapter_resolves_with_pressed_choice(monkeypatch):
    # The secondary-profile rebuild path: adapter A sends the card, then a retryable fatal makes
    # _secondary_reconnect_attempt build adapter B with A as the predecessor. A press on B must still
    # resolve the pending request with the pressed choice, not wait for /stop.
    from gateway.config import Platform
    from tests.gateway.test_multiplex_adapter_registry import (
        _install_secondary_reconnect_context,
        _secondary_recovery_runner,
    )
    created: list[_FakeBot] = []
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    old = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    await _connect(old, monkeypatch, created)
    old._allowed_user_ids = {"11111"}
    card = _capture_card(created[0])
    assert (await old.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)).success is True

    new = DiscordAdapter(PlatformConfig(enabled=True, token="test-token", extra={"allow_from": ["11111"]}))
    await _connect(new, monkeypatch, created)
    runner = _secondary_recovery_runner()
    _install_secondary_reconnect_context(monkeypatch, runner, new)
    from gateway.config import GatewayConfig
    monkeypatch.setattr("gateway.config.load_gateway_config", lambda: GatewayConfig(
        multiplex_profiles=True,
        platforms={Platform.DISCORD: PlatformConfig(enabled=True, token="test-token", extra={"allow_from": ["11111"]})}))
    adapter, success = await runner._secondary_reconnect_attempt("reviewer", Platform.DISCORD, old)
    assert adapter is new
    assert success is True

    custom_id = f"hermes:exec:{request_id}:once"
    interaction, _response = _press(custom_id, message=card["message"])
    await created[-1]._events["on_interaction"](interaction)

    assert entry.result == "once"
    assert entry.event.is_set()
    await new.disconnect()


@pytest.mark.asyncio
async def test_rebuilt_adapter_allowlist_reaches_adopted_card(monkeypatch):
    # The rebuild carries the old card view, so the view must take the rebuilt adapter's allowlist. A user
    # the new adapter admits must be able to answer an adopted card, not be refused by the old snapshot.
    for name in ("DISCORD_ALLOW_ALL_USERS", "GATEWAY_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS"):
        monkeypatch.delenv(name, raising=False)
    created: list[_FakeBot] = []
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    entry.authority = {"requester_id": "22222", "approvers": ["22222"]}

    old = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    await _connect(old, monkeypatch, created)
    old._allowed_user_ids = {"11111"}
    card = _capture_card(created[0])
    assert (await old.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)).success is True

    new = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    await _connect(new, monkeypatch, created)
    new._allowed_user_ids = {"22222"}
    new.adopt_exec_approval_views(old)

    custom_id = f"hermes:exec:{request_id}:once"
    interaction, _response = _press(custom_id, message=card["message"], user_id="22222")
    await created[-1]._events["on_interaction"](interaction)

    assert entry.result == "once"
    assert entry.event.is_set()
    await old.disconnect()
    await new.disconnect()


@pytest.mark.asyncio
async def test_adopted_card_follows_the_rebuilt_allowlist(monkeypatch):
    # connect() derives the rebuilt adapter's allowlist from its config, after the carry has run. The adopted card
    # reads that list at press time: 33333 is added by the rebuild and admitted, 11111 is removed and refused.
    for name in ("DISCORD_ALLOW_ALL_USERS", "GATEWAY_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS", "DISCORD_ALLOWED_USERS"):
        monkeypatch.delenv(name, raising=False)
    created: list[_FakeBot] = []
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    entry.authority = {"requester_id": "11111", "approvers": ["11111", "33333"]}

    old = DiscordAdapter(PlatformConfig(enabled=True, token="test-token", extra={"allow_from": ["11111"]}))
    await _connect(old, monkeypatch, created)
    card = _capture_card(created[0])
    assert (await old.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)).success is True

    new = DiscordAdapter(PlatformConfig(enabled=True, token="test-token", extra={"allow_from": ["33333"]}))
    new.adopt_exec_approval_views(old)  # the carry runs before connect(), as in the runner
    await _connect(new, monkeypatch, created)
    assert new._allowed_user_ids == {"33333"}

    custom_id = f"hermes:exec:{request_id}:once"
    removed, removed_response = _press(custom_id, message=card["message"], user_id="11111")
    await created[-1]._events["on_interaction"](removed)
    assert entry.result is None
    removed_response.send_message.assert_awaited_once()
    assert removed_response.send_message.await_args.args[0] == discord_platform._unauthorized()

    added, _added_response = _press(custom_id, message=card["message"], user_id="33333")
    await created[-1]._events["on_interaction"](added)
    assert entry.result == "once"
    assert entry.event.is_set()
    await old.disconnect()
    await new.disconnect()


@pytest.mark.asyncio
async def test_unmatched_press_resolves_nothing_when_request_moved_to_another_session(monkeypatch):
    # The card's request now sits in a different session's queue. A press on the card must not settle it through
    # the card's own session, and must answer nothing.
    for name in ("DISCORD_ALLOW_ALL_USERS", "GATEWAY_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS"):
        monkeypatch.delenv(name, raising=False)
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}

    session = "agent:other:discord:555"
    other = "agent:other:discord:777"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    card = _capture_card(created[0])
    assert (await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)).success is True
    await card["view"].retire("resolved")
    entry.result = None
    monkeypatch.setitem(approval._gateway_queues, session, [])
    monkeypatch.setitem(approval._gateway_queues, other, [entry])

    on_interaction = created[0]._events["on_interaction"]
    interaction, _response = _press(f"hermes:exec:{request_id}:once", message=card["message"], user_id="11111")
    await on_interaction(interaction)

    assert entry.result is None
    assert not entry.event.is_set()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_adopted_card_follows_the_rebuilt_role_policy(monkeypatch):
    # The rebuild drops role 42 from the allowlist and adds role 77. The adopted card reads the rebuilt adapter's role
    # policy at press time: a clicker holding only the removed role is refused, one holding the added role is admitted.
    for name in ("DISCORD_ALLOW_ALL_USERS", "GATEWAY_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS",
                 "DISCORD_ALLOWED_USERS", "DISCORD_ALLOWED_ROLES"):
        monkeypatch.delenv(name, raising=False)
    created: list[_FakeBot] = []
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    entry.authority = {"requester_id": "11111", "approvers": ["11111", "33333", "44444"]}

    old = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                        extra={"allow_from": ["11111"], "allowed_roles": ["42"]}))
    await _connect(old, monkeypatch, created)
    card = _capture_card(created[0])
    assert (await old.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)).success is True

    new = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                        extra={"allow_from": ["11111"], "allowed_roles": ["77"]}))
    new.adopt_exec_approval_views(old)  # the carry runs before connect(), as in the runner
    await _connect(new, monkeypatch, created)
    assert new._allowed_role_ids == {77}

    custom_id = f"hermes:exec:{request_id}:once"
    removed, removed_response = _press(custom_id, message=card["message"], user_id="33333")
    removed.user.roles = [SimpleNamespace(id=42)]
    await created[-1]._events["on_interaction"](removed)
    assert entry.result is None
    removed_response.send_message.assert_awaited_once()
    assert removed_response.send_message.await_args.args[0] == discord_platform._unauthorized()

    added, _added_response = _press(custom_id, message=card["message"], user_id="44444")
    added.user.roles = [SimpleNamespace(id=77)]
    await created[-1]._events["on_interaction"](added)
    assert entry.result == "once"
    assert entry.event.is_set()
    await old.disconnect()
    await new.disconnect()


@pytest.mark.asyncio
async def test_adopted_card_follows_the_rebuilt_admin_gate(monkeypatch):
    # The rebuild turns require_admin_for_exec_approval on, with 44444 the only admin. The adopted card reads that gate at
    # press time: an admitted approver who is not an admin is refused and the request stays pending; the admin resolves it.
    for name in ("DISCORD_ALLOW_ALL_USERS", "GATEWAY_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS",
                 "DISCORD_ALLOWED_USERS", "DISCORD_ALLOWED_ROLES"):
        monkeypatch.delenv(name, raising=False)
    created: list[_FakeBot] = []
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    entry.authority = {"requester_id": "11111", "approvers": ["11111", "33333"]}

    old = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                        extra={"allow_from": ["11111", "33333", "44444"]}))
    await _connect(old, monkeypatch, created)
    card = _capture_card(created[0])
    assert (await old.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)).success is True

    new = DiscordAdapter(PlatformConfig(enabled=True, token="test-token", extra={
        "allow_from": ["11111", "33333", "44444"],
        "require_admin_for_exec_approval": True, "allow_admin_from": ["44444"]}))
    new.adopt_exec_approval_views(old)  # the carry runs before connect(), as in the runner
    await _connect(new, monkeypatch, created)
    assert new._allowed_user_ids == {"11111", "33333", "44444"}

    custom_id = f"hermes:exec:{request_id}:once"
    refused, refused_response = _press(custom_id, message=card["message"], user_id="33333")
    await created[-1]._events["on_interaction"](refused)
    assert entry.result is None
    assert not entry.event.is_set()
    assert request_id in [e.data.get("request_id") for e in approval._gateway_queues.get(session, [])]
    refused_response.send_message.assert_awaited_once()
    assert refused_response.send_message.await_args.args[0] == discord_platform._unauthorized()

    admitted, _admitted_response = _press(custom_id, message=card["message"], user_id="44444")
    await created[-1]._events["on_interaction"](admitted)
    assert entry.result == "once"
    assert entry.event.is_set()
    await old.disconnect()
    await new.disconnect()


@pytest.mark.asyncio
async def test_adopted_card_takes_the_rebuilt_admin_list_for_a_non_requester(monkeypatch):
    # The rebuild adds 44444 to allow_admin_from with the admin gate off. The adopted card answers through the rebuilt
    # platform extra, so 44444 may answer although they are neither the requester nor an approver. The predecessor's
    # extra listed no admin, so the press would be refused if the view kept its own extra.
    for name in ("DISCORD_ALLOW_ALL_USERS", "GATEWAY_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS",
                 "DISCORD_ALLOWED_USERS", "DISCORD_ALLOWED_ROLES"):
        monkeypatch.delenv(name, raising=False)
    created: list[_FakeBot] = []
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    entry.authority = {"requester_id": "11111", "approvers": ["11111"]}

    old = DiscordAdapter(PlatformConfig(enabled=True, token="test-token", extra={"allow_from": ["11111", "44444"]}))
    await _connect(old, monkeypatch, created)
    card = _capture_card(created[0])
    assert (await old.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id)).success is True

    new = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                        extra={"allow_from": ["11111", "44444"], "allow_admin_from": ["44444"]}))
    new.adopt_exec_approval_views(old)  # the carry runs before connect(), as in the runner
    await _connect(new, monkeypatch, created)

    custom_id = f"hermes:exec:{request_id}:once"
    interaction, _response = _press(custom_id, message=card["message"], user_id="44444")
    await created[-1]._events["on_interaction"](interaction)
    assert entry.result == "once"
    assert entry.event.is_set()
    await old.disconnect()
    await new.disconnect()


@pytest.mark.asyncio
async def test_card_carried_across_reconnect_is_disabled_when_its_wait_times_out(monkeypatch):
    """A card sent before a reconnect is carried to the rebuilt adapter. When its wait then ends by timeout, the
    card's buttons must be disabled through the rebuilt client, and a later press must get the expired reply."""
    from tools import approval_context
    from tools.approval_gateway_wait import _await_gateway_decision

    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    adapter._allowed_user_ids = {"11111"}
    monkeypatch.setattr(approval_context, "_get_approval_timeout", lambda: 1)
    monkeypatch.setattr(approval, "_gateway_queues", {})

    session = "agent:a:discord:555"
    card = _capture_card(created[0], message_id=3333)
    loop = asyncio.get_running_loop()

    def notify(data):
        future = asyncio.run_coroutine_threadsafe(
            adapter.send_exec_approval(chat_id="555", command="rm x", session_key=session,
                                       request_id=data["request_id"]),
            loop)
        assert future.result(5).success is True

    wait = asyncio.create_task(asyncio.to_thread(
        _await_gateway_decision, session, notify, {"command": "rm x", "pattern_keys": ["k"]}))
    while "view" not in card:
        await asyncio.sleep(0.01)
    view = card["view"]
    request_id = view.request_id

    # Reconnect: the old client's session closes, so an edit through the old message handle fails from now on.
    # Each client serves the card's channel and hands back a message bound to its own session.
    old_session_open = {"open": True}
    landed: list[dict] = []
    old_message = card["message"]
    old_message.channel = SimpleNamespace(id=555)  # a real discord.Message carries its channel

    async def old_edit(**kwargs):
        if not old_session_open["open"]:
            raise RuntimeError("Session is closed")
        landed.append({"client": "old", **kwargs})

    old_message.edit = AsyncMock(side_effect=old_edit)

    rebuilt = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    rebuilt.adopt_exec_approval_views(adapter)
    await adapter.disconnect()
    old_session_open["open"] = False

    # A real PartialMessage has channel and id, and no embeds. A real client has no cached channels before READY,
    # so get_channel returns None while get_partial_messageable still works.
    new_message = SimpleNamespace(id=3333, channel=SimpleNamespace(id=555), edit=AsyncMock())

    async def new_edit(**kwargs):
        landed.append({"client": "new", **kwargs})

    new_message.edit.side_effect = new_edit
    partial = SimpleNamespace(get_partial_message=lambda _id: new_message)
    monkeypatch.setattr(_FakeBot, "get_partial_messageable", lambda self, _channel_id, **_kw: partial, raising=False)
    await _connect(rebuilt, monkeypatch, created)
    rebuilt._allowed_user_ids = {"11111"}

    decision = await asyncio.wait_for(wait, 10)
    assert decision["resolved"] is False
    await asyncio.sleep(0)
    await asyncio.gather(*rebuilt._background_tasks)

    # The card's buttons are disabled through the rebuilt client: an edit with no view lands there.
    assert view.is_finished()
    assert any(e["client"] == "new" and e.get("view") is None for e in landed)
    # The expiry edit keeps the card's embed: greyed out and stamped with the expired footer, not replaced by None.
    expiry = next(e for e in landed if e["client"] == "new" and e.get("view") is None)
    assert expiry["embed"] is card["embed"]
    assert expiry["embed"].color == discord_platform.discord.Color.greyple()
    footer = expiry["embed"].footer
    assert (footer["text"] if isinstance(footer, dict) else footer.text) == t("platform.discord.prompt.expired_footer")

    # A late press on that card now gets the expired reply.
    custom_id = f"hermes:exec:{request_id}:once"
    on_interaction = created[-1]._events["on_interaction"]
    interaction, response = _press(custom_id, message=new_message)
    await on_interaction(interaction)
    response.send_message.assert_awaited_once()
    assert response.send_message.await_args.args[0] == t("platform.discord.approval.no_longer_pending")
    await rebuilt.disconnect()


def _channel_for(channel_id: int, message_id: int, *, send_error=None, hang_cards: bool = False):
    """A channel whose sends return a message that carries this channel. ``send_error`` makes every send raise.
    ``hang_cards`` makes card sends (kwargs with a ``view``) never return; plain-text sends still succeed."""
    async def send(**kwargs):
        if send_error is not None:
            raise send_error
        if hang_cards and "view" in kwargs:
            await asyncio.Event().wait()
        return SimpleNamespace(id=message_id, channel=SimpleNamespace(id=channel_id), edit=AsyncMock(),
                               embeds=[kwargs["embed"]] if "embed" in kwargs else [])
    return SimpleNamespace(id=channel_id, send=AsyncMock(side_effect=send))


def _text(content: str, author_id: str, channel_id: int):
    """A plain Discord message from ``author_id`` in channel ``channel_id`` (a thread, or its parent)."""
    return SimpleNamespace(id=uuid.uuid4().int % 10**15, content=content,
                           author=SimpleNamespace(id=author_id), channel=SimpleNamespace(id=channel_id))


async def _answer(adapter, monkeypatch, message) -> AsyncMock:
    """Deliver ``message`` through the adapter's live inbound path, admitted, and return the normal-turn handler:
    a text answer the approval gate does not take reaches it unchanged."""
    monkeypatch.setattr(adapter, "_discord_message_admission", lambda _m, *, claim: (True, False))
    handle = AsyncMock(return_value=True)
    monkeypatch.setattr(adapter, "_handle_message", handle)
    await adapter._dispatch_discord_message(message)
    return handle


async def _until(predicate, timeout: float = 5.0) -> bool:
    """Poll ``predicate`` on the loop until it holds or ``timeout`` passes."""
    deadline = time.monotonic() + timeout
    while not predicate():
        if time.monotonic() > deadline:
            return False
        await asyncio.sleep(0.01)
    return True


@pytest.mark.asyncio
async def test_card_undelivered_before_drop_is_posted_again_in_its_thread_after_reconnect(monkeypatch):
    # The connection drops before the card reaches Discord: the send fails, the request stays pending. On reconnect
    # the card is posted again in its own thread, with the approval mention, and is tracked as sent.
    monkeypatch.setenv("DISCORD_ALLOWED_USERS", "11111")
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                            extra={"approval_mentions": True, "approval_card_confirm_timeout": 5}))
    created: list[_FakeBot] = []
    dropped = _channel_for(777, 1, send_error=ConnectionError("gateway connection dropped"))
    thread = _channel_for(777, 2)
    monkeypatch.setattr(_FakeBot, "get_channel", lambda self, _cid: dropped if self is created[0] else thread)
    await _connect(adapter, monkeypatch, created)

    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id, metadata={"thread_id": "777"})
    assert result.success is False
    assert entry.result is None and not entry.event.is_set()

    await _connect(adapter, monkeypatch, created)
    assert await _until(lambda: thread.send.await_count == 1)
    kwargs = thread.send.await_args.kwargs
    assert "<@11111>" in kwargs["content"]
    assert adapter._exec_approval_views[request_id] is kwargs["view"]
    assert kwargs["view"]._message is not None
    assert entry.result is None and not entry.event.is_set()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_card_not_confirmed_in_time_ends_the_wait_and_posts_the_fallback_in_the_thread(monkeypatch):
    # The card send never confirms within the configured window. The turn must see "card not delivered" at once
    # instead of hanging to the approval timeout, and the plain-text fallback goes to the approval's thread.
    from tools import approval_context
    from tools.approval_gateway_wait import _await_gateway_decision

    monkeypatch.setenv("DISCORD_ALLOWED_USERS", "11111")
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                            extra={"approval_card_confirm_timeout": 0.2}))
    created: list[_FakeBot] = []
    thread = _channel_for(777, 1, hang_cards=True)
    monkeypatch.setattr(_FakeBot, "get_channel", lambda self, _cid: thread)
    await _connect(adapter, monkeypatch, created)
    monkeypatch.setattr(approval_context, "_get_approval_timeout", lambda: 30)
    monkeypatch.setattr(approval, "_gateway_queues", {})
    session = "agent:a:discord:555"
    loop = asyncio.get_running_loop()

    def notify(data):
        # Non-blocking, as the runner's notify: the turn keeps waiting while the card send is in flight.
        asyncio.run_coroutine_threadsafe(
            adapter.send_exec_approval(chat_id="555", command="rm x", session_key=session,
                                       request_id=data["request_id"], metadata={"thread_id": "777"}),
            loop)

    decision = await asyncio.wait_for(asyncio.to_thread(
        _await_gateway_decision, session, notify, {"command": "rm x", "pattern_keys": ["k"]}), 10)

    # A withdrawal, not an answer: no choice, and the cause names the undelivered card.
    assert decision["choice"] is None
    assert decision.get("cancelled") == "the approval card was not delivered"
    fallback = [call.kwargs for call in thread.send.await_args_list if "view" not in call.kwargs]
    assert len(fallback) == 1 and "rm x" in fallback[0]["content"]
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_undelivered_card_fallback_names_the_confirm_window_it_is_withdrawn_after(monkeypatch):
    # The card never reaches Discord, so the request is withdrawn after approval_card_confirm_timeout. The text
    # fallback must name that window. approvals.timeout (1 hour here) is not the deadline on this path.
    from tools import approval_context

    monkeypatch.setenv("DISCORD_ALLOWED_USERS", "11111")
    monkeypatch.setattr(approval_context, "_get_approval_timeout", lambda: 3600)
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                            extra={"approval_card_confirm_timeout": 30}))
    created: list[_FakeBot] = []

    async def send(**kwargs):
        if "view" in kwargs:  # the card fails; the plain-text fallback goes through
            raise ConnectionError("gateway connection dropped")
        return SimpleNamespace(id=2, channel=SimpleNamespace(id=777), edit=AsyncMock(), embeds=[])

    thread = SimpleNamespace(id=777, send=AsyncMock(side_effect=send))
    monkeypatch.setattr(_FakeBot, "get_channel", lambda self, _cid: thread)
    await _connect(adapter, monkeypatch, created)
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    _queue_entry(monkeypatch, session, request_id)
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id, metadata={"thread_id": "777"})
    assert result.raw_response == {"approval_text_fallback_posted": True}
    fallback = [call.kwargs for call in thread.send.await_args_list if "view" not in call.kwargs]
    assert len(fallback) == 1
    assert "within 30 seconds it will NOT run" in fallback[0]["content"]
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_text_approve_from_a_non_approver_or_another_thread_resolves_nothing(monkeypatch):
    monkeypatch.setenv("DISCORD_ALLOWED_USERS", "11111")
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                            extra={"approval_text_approver_id": "22222"}))
    created: list[_FakeBot] = []
    monkeypatch.setattr(_FakeBot, "get_channel", lambda self, cid: _channel_for(cid, 1))
    await _connect(adapter, monkeypatch, created)
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id, metadata={"thread_id": "777"})
    assert result.success is True

    # The requester is not the configured approver: consumed in the approval's thread, resolves nothing. The
    # approver's word in another thread matches no approval and flows on.
    assert (await _answer(adapter, monkeypatch, _text("approve", "11111", 777))).await_count == 0
    assert (await _answer(adapter, monkeypatch, _text("approve", "22222", 888))).await_count == 1
    assert entry.result is None and not entry.event.is_set()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_text_approve_from_the_approver_in_the_approval_thread_resolves_it(monkeypatch):
    monkeypatch.setenv("DISCORD_ALLOWED_USERS", "11111")
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                            extra={"approval_text_approver_id": "22222"}))
    created: list[_FakeBot] = []
    monkeypatch.setattr(_FakeBot, "get_channel", lambda self, cid: _channel_for(cid, 1))
    await _connect(adapter, monkeypatch, created)
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id, metadata={"thread_id": "777"})
    assert result.success is True

    handle = await _answer(adapter, monkeypatch, _text("approve", "22222", 777))
    handle.assert_not_awaited()
    assert entry.result == "once"
    assert entry.event.is_set()
    await adapter.disconnect()


@pytest.mark.asyncio
@pytest.mark.parametrize("extra", [{}, {"approval_text_approver_id": ""}])
async def test_text_answers_are_disabled_when_no_approver_is_configured(monkeypatch, extra):
    # Fail closed: with no configured approver, no author may answer by text, not even the requester.
    monkeypatch.setenv("DISCORD_ALLOWED_USERS", "11111")
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token", extra=extra))
    created: list[_FakeBot] = []
    monkeypatch.setattr(_FakeBot, "get_channel", lambda self, cid: _channel_for(cid, 1))
    await _connect(adapter, monkeypatch, created)
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id, metadata={"thread_id": "777"})
    assert result.success is True

    assert (await _answer(adapter, monkeypatch, _text("approve", "22222", 777))).await_count == 0
    assert (await _answer(adapter, monkeypatch, _text("approve", "11111", 777))).await_count == 0
    assert entry.result is None and not entry.event.is_set()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_text_answer_in_a_thread_with_two_pending_approvals_resolves_nothing(monkeypatch):
    # Exactly one pending request must match the thread; with two, the text answer is ignored.
    monkeypatch.setenv("DISCORD_ALLOWED_USERS", "11111")
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                            extra={"approval_text_approver_id": "22222"}))
    created: list[_FakeBot] = []
    monkeypatch.setattr(_FakeBot, "get_channel", lambda self, cid: _channel_for(cid, 1))
    await _connect(adapter, monkeypatch, created)
    session = "agent:other:discord:555"
    first_id, second_id = uuid.uuid4().hex, uuid.uuid4().hex
    first = _ApprovalEntry({"request_id": first_id, "command": "rm a"})
    first.authority = {"requester_id": "11111"}
    second = _ApprovalEntry({"request_id": second_id, "command": "rm b"})
    second.authority = {"requester_id": "11111"}
    monkeypatch.setitem(approval._gateway_queues, session, [first, second])
    for request_id, command in ((first_id, "rm a"), (second_id, "rm b")):
        result = await adapter.send_exec_approval(
            chat_id="555", command=command, session_key=session, request_id=request_id, metadata={"thread_id": "777"})
        assert result.success is True

    assert (await _answer(adapter, monkeypatch, _text("approve", "22222", 777))).await_count == 0
    assert first.result is None and second.result is None
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_card_not_confirmed_in_time_is_answered_by_the_approver_in_the_thread_within_the_window(monkeypatch):
    # The card never confirms, the fallback goes to the thread, and the approver's "approve" there within N resolves.
    from tools import approval_context
    from tools.approval_gateway_wait import _await_gateway_decision

    monkeypatch.setenv("DISCORD_ALLOWED_USERS", "11111")
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token", extra={
        "approval_card_confirm_timeout": 2, "approval_text_approver_id": "22222"}))
    created: list[_FakeBot] = []
    thread = _channel_for(777, 1, hang_cards=True)
    monkeypatch.setattr(_FakeBot, "get_channel", lambda self, _cid: thread)
    await _connect(adapter, monkeypatch, created)
    monkeypatch.setattr(approval_context, "_get_approval_timeout", lambda: 30)
    monkeypatch.setattr(approval, "_gateway_queues", {})
    session = "agent:a:discord:555"
    loop = asyncio.get_running_loop()

    def notify(data):
        asyncio.run_coroutine_threadsafe(
            adapter.send_exec_approval(chat_id="555", command="rm x", session_key=session,
                                       request_id=data["request_id"], metadata={"thread_id": "777"}),
            loop)

    wait = asyncio.ensure_future(asyncio.to_thread(
        _await_gateway_decision, session, notify, {"command": "rm x", "pattern_keys": ["k"]}))
    assert await _until(lambda: any("view" not in c.kwargs for c in thread.send.await_args_list))
    handle = await _answer(adapter, monkeypatch, _text("approve", "22222", 777))
    handle.assert_not_awaited()

    decision = await asyncio.wait_for(wait, 10)
    assert decision["resolved"] is True and decision["choice"] == "once"
    await adapter.disconnect()


async def _gated_adapter(monkeypatch, *, extra=None):
    """A connected adapter with ``22222`` as the text approver and one pending card in thread 777."""
    monkeypatch.setenv("DISCORD_ALLOWED_USERS", "11111")
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                            extra={"approval_text_approver_id": "22222", **(extra or {})}))
    created: list[_FakeBot] = []
    monkeypatch.setattr(_FakeBot, "get_channel", lambda self, cid: _channel_for(cid, 1))
    await _connect(adapter, monkeypatch, created)
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id, metadata={"thread_id": "777"})
    assert result.success is True
    return adapter, session, entry


def _busy_runner(adapter):
    """A runner stub whose real busy route reaches ``adapter``; the oldest-request fallback is a mock."""
    from gateway.run_busy import GatewayBusySessionMixin, approval_input_words
    words = {w: (k, "") for k in ("approve", "deny") for w in approval_input_words(k)}
    runner = SimpleNamespace(_delivery_adapter_for=lambda _source: adapter, _plaintext_approval_words=lambda: words,
                             _send_busy_reply=AsyncMock(),
                             _handle_approve_command=AsyncMock(return_value=None),
                             _handle_deny_command=AsyncMock(return_value=None))
    route = GatewayBusySessionMixin._route_plaintext_approval_while_busy
    return runner, lambda event, key: route(runner, event, key)


def _busy_event(text, user_id, thread_id):
    from gateway.platforms.event import MessageEvent
    from gateway.session import SessionSource
    from gateway.config import Platform
    source = SessionSource(platform=Platform.DISCORD, chat_id=str(thread_id), chat_type="thread",
                           user_id=user_id, thread_id=str(thread_id))
    return MessageEvent(text=text, message_id="m1", source=source)


@pytest.mark.asyncio
@pytest.mark.parametrize("word", ["approve", "deny"])
async def test_a1_non_approver_approval_word_in_the_approval_thread_is_consumed_and_resolves_nothing(
        monkeypatch, word):
    adapter, session, entry = await _gated_adapter(monkeypatch)
    handle = await _answer(adapter, monkeypatch, _text(word, "11111", 777))
    handle.assert_not_awaited()  # consumed: never reaches the busy route through _handle_message
    runner, route = _busy_runner(adapter)
    assert await route(_busy_event(word, "11111", 777), session) is True
    runner._handle_approve_command.assert_not_awaited()
    assert entry.result is None and not entry.event.is_set()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_b1_approver_approve_in_another_thread_is_not_consumed_and_resolves_nothing(monkeypatch):
    adapter, session, entry = await _gated_adapter(monkeypatch)
    handle = await _answer(adapter, monkeypatch, _text("approve", "22222", 888))
    assert handle.await_count == 1
    runner, route = _busy_runner(adapter)
    assert await route(_busy_event("approve", "22222", 888), session) is False
    runner._handle_approve_command.assert_not_awaited()
    assert entry.result is None and not entry.event.is_set()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_c1_backfilled_non_approver_approve_goes_through_the_gate(monkeypatch):
    adapter, _session, entry = await _gated_adapter(monkeypatch)
    monkeypatch.setattr(adapter, "_discord_message_admission", lambda _m, *, claim: (True, False))
    handle = AsyncMock(return_value=True)
    monkeypatch.setattr(adapter, "_handle_message", handle)
    message = _text("approve", "11111", 777)
    monkeypatch.setattr(adapter, "_discord_require_mention", lambda: False)
    monkeypatch.setattr(adapter, "_get_parent_channel_id", lambda _c: None)
    await adapter._dispatch_recovered_message(message)
    handle.assert_not_awaited()
    assert entry.result is None and not entry.event.is_set()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_d1_d3_typed_answer_on_a_deny_only_request(monkeypatch):
    adapter, _session, entry = await _gated_adapter(monkeypatch)
    entry.card_deny_only = True
    (await _answer(adapter, monkeypatch, _text("approve", "22222", 777))).assert_not_awaited()
    assert entry.result is None and not entry.event.is_set()
    (await _answer(adapter, monkeypatch, _text("deny", "22222", 777))).assert_not_awaited()
    assert entry.result == "deny"
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_d2_typed_approve_on_a_smart_denied_request_resolves_nothing_but_deny_does(monkeypatch):
    adapter, _session, entry = await _gated_adapter(monkeypatch)
    entry.data["smart_denied"] = True
    (await _answer(adapter, monkeypatch, _text("approve", "22222", 777))).assert_not_awaited()
    assert entry.result is None and not entry.event.is_set()
    (await _answer(adapter, monkeypatch, _text("deny", "22222", 777))).assert_not_awaited()
    assert entry.result == "deny"
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_e1_busy_route_approver_approve_in_the_approval_thread_resolves_once(monkeypatch):
    adapter, session, entry = await _gated_adapter(monkeypatch)
    runner, route = _busy_runner(adapter)
    assert await route(_busy_event("approve", "22222", 777), session) is True
    runner._handle_approve_command.assert_not_awaited()
    assert entry.result == "once"
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_w1_withdrawal_fires_even_when_the_fallback_send_hangs(monkeypatch):
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                            extra={"approval_card_confirm_timeout": 0.2}))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    channel = SimpleNamespace(send=AsyncMock(side_effect=RuntimeError("send failed")))
    created[0].get_channel = lambda _cid: channel
    monkeypatch.setattr(adapter, "_send_exec_approval_text_fallback",
                        lambda _p: asyncio.Event().wait())
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    send = asyncio.ensure_future(adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id))
    assert await _until(lambda: entry.event.is_set(), timeout=3)
    assert entry.cancelled == "the approval card was not delivered"
    send.cancel()
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_r1_repost_that_hangs_past_the_window_posts_no_second_card(monkeypatch):
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                            extra={"approval_card_confirm_timeout": 0.2}))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    monkeypatch.setattr(adapter, "_EXEC_APPROVAL_REPOST_BACKOFF", (0.0, 0.0, 0.0))
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    _queue_entry(monkeypatch, session, request_id)
    view = adapter._new_exec_approval_view(SimpleNamespace(
        session_key=session, request_id=request_id, choices=("once", "deny"), smart_denied=False,
        chat_id="555", metadata={"thread_id": "777"}))
    view.undelivered = True
    calls = []

    async def hang(_prompt, _view):
        calls.append(1)
        await asyncio.Event().wait()
    monkeypatch.setattr(adapter, "_send_exec_approval_card", hang)
    await asyncio.wait_for(adapter._repost_undelivered_exec_approval_cards(), 5)
    assert len(calls) == 1
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_failed_repost_keeps_the_replacement_retryable(monkeypatch):
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                            extra={"approval_card_confirm_timeout": 0.2}))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    monkeypatch.setattr(adapter, "_EXEC_APPROVAL_REPOST_BACKOFF", (0.0,))
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    _queue_entry(monkeypatch, session, request_id)
    prompt = SimpleNamespace(session_key=session, request_id=request_id, choices=("once", "deny"),
                             smart_denied=False, chat_id="555", metadata={"thread_id": "777"})
    adapter._new_exec_approval_view(prompt).undelivered = True
    from gateway.platforms.base import SendResult
    monkeypatch.setattr(adapter, "_send_exec_approval_card",
                        AsyncMock(return_value=SendResult(success=False, error="down")))
    await adapter._repost_undelivered_exec_approval_cards()
    replacement = adapter._exec_approval_views[request_id]
    assert replacement.undelivered is True and replacement.prompt is prompt
    await adapter.disconnect()


@pytest.mark.asyncio
@pytest.mark.parametrize("text", ["yes", "👍", "always", "please approve", "approve it"])
async def test_only_exact_approve_or_deny_is_swallowed_other_text_passes_through(monkeypatch, text):
    # The gate swallows only an exact approve or deny. Any other text in the approval's thread, including the
    # busy route's other words and sentences, reaches the normal path unchanged and resolves nothing.
    monkeypatch.setenv("DISCORD_ALLOWED_USERS", "11111")
    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                            extra={"approval_text_approver_id": "22222"}))
    created: list[_FakeBot] = []
    monkeypatch.setattr(_FakeBot, "get_channel", lambda self, cid: _channel_for(cid, 1))
    await _connect(adapter, monkeypatch, created)
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)
    result = await adapter.send_exec_approval(
        chat_id="555", command="rm x", session_key=session, request_id=request_id, metadata={"thread_id": "777"})
    assert result.success is True

    handle = await _answer(adapter, monkeypatch, _text(text, "22222", 777))
    assert handle.await_count == 1
    assert entry.result is None and not entry.event.is_set()
    await adapter.disconnect()


def _undelivered_prompt(session, request_id):
    from gateway.platforms.base import ExecApprovalPrompt
    return ExecApprovalPrompt(
        chat_id="555", session_key=session, text="rm x", command="rm x", smart_denied=False,
        actions=[("Allow once", "once", "primary"), ("Deny", "deny", "danger")],
        request_id=request_id, metadata={"thread_id": "777"})


def _gated_text_fallback(monkeypatch, adapter):
    """Hold the text fallback until the test releases it, then it fails. Returns (started, release)."""
    started, release = asyncio.Event(), asyncio.Event()

    async def fallback(_prompt):
        started.set()
        await release.wait()
        return "send_path_degraded"
    monkeypatch.setattr(adapter, "_send_exec_approval_text_fallback", fallback)
    return started, release


@pytest.mark.asyncio
async def test_repost_that_fails_once_then_shows_the_card_clears_the_never_shown_mark(monkeypatch):
    # The card and its text fallback both fail, so the request is marked never shown. Repost 1 fails, repost 2
    # shows the card. The mark must come off, or a /stop would say the card was never shown.
    from gateway.platforms.base import SendResult

    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                            extra={"approval_card_confirm_timeout": 5}))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    monkeypatch.setattr(adapter, "_EXEC_APPROVAL_REPOST_BACKOFF", (0.0, 0.0))
    monkeypatch.setattr(adapter, "_send_exec_approval_text_fallback", AsyncMock(return_value="send_path_degraded"))
    card = AsyncMock(side_effect=[
        SendResult(success=False, error="Not connected"),  # the card
        SendResult(success=False, error="down"),  # repost 1
        SendResult(success=True, message_id="3"),  # repost 2
    ])
    monkeypatch.setattr(adapter, "_send_exec_approval_card", card)
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    await adapter._send_exec_approval_prompt(_undelivered_prompt(session, request_id))
    assert entry.undelivered == "Not connected, then send_path_degraded"
    await asyncio.wait_for(adapter._repost_undelivered_exec_approval_cards(), 5)

    assert card.await_count == 3
    assert entry.undelivered is None
    assert not getattr(adapter._exec_approval_views[request_id], "undelivered", False)
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_text_fallback_failing_after_a_repost_showed_the_card_does_not_mark_never_shown(monkeypatch):
    # The card fails, the text fallback is still in flight, and a reconnect repost shows the card. When the
    # fallback then fails, the card was shown, so the request must not be marked never shown.
    from gateway.platforms.base import SendResult

    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                            extra={"approval_card_confirm_timeout": 5}))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    monkeypatch.setattr(adapter, "_EXEC_APPROVAL_REPOST_BACKOFF", (0.0,))
    started, release = _gated_text_fallback(monkeypatch, adapter)
    monkeypatch.setattr(adapter, "_send_exec_approval_card", AsyncMock(side_effect=[
        SendResult(success=False, error="Not connected"),  # the card
        SendResult(success=True, message_id="2"),  # the repost
    ]))
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    prompt_task = asyncio.create_task(adapter._send_exec_approval_prompt(_undelivered_prompt(session, request_id)))
    await asyncio.wait_for(started.wait(), 5)
    await asyncio.wait_for(adapter._repost_undelivered_exec_approval_cards(), 5)
    assert entry.undelivered is None
    release.set()
    await asyncio.wait_for(prompt_task, 5)

    assert entry.undelivered is None
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_repost_failing_every_attempt_keeps_the_request_never_shown_with_the_card_failure(monkeypatch):
    # The card and its text fallback fail, then every repost attempt fails too. No surface ever showed the prompt,
    # so the mark stays, and its reason is the card's failure followed by the fallback's.
    from gateway.platforms.base import SendResult

    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                            extra={"approval_card_confirm_timeout": 5}))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    monkeypatch.setattr(adapter, "_EXEC_APPROVAL_REPOST_BACKOFF", (0.0, 0.0, 0.0))
    monkeypatch.setattr(adapter, "_send_exec_approval_text_fallback", AsyncMock(return_value="send_path_degraded"))
    card = AsyncMock(side_effect=[SendResult(success=False, error="Not connected")]
                     + [SendResult(success=False, error="down") for _ in range(3)])
    monkeypatch.setattr(adapter, "_send_exec_approval_card", card)
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    await adapter._send_exec_approval_prompt(_undelivered_prompt(session, request_id))
    await asyncio.wait_for(adapter._repost_undelivered_exec_approval_cards(), 5)

    assert card.await_count == 4
    assert entry.undelivered == "Not connected, then send_path_degraded"
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_fallback_for_a_card_a_later_repost_showed_does_not_mark_the_request(monkeypatch):
    # F4, stale view. The card fails and its text fallback is in flight. Repost 1 fails and registers a replacement
    # view for the request. Repost 2 shows the card on that replacement. The original card's flag was never
    # cleared, so a fallback that reads it would mark a shown card as never shown.
    from gateway.platforms.base import SendResult

    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                            extra={"approval_card_confirm_timeout": 5}))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    monkeypatch.setattr(adapter, "_EXEC_APPROVAL_REPOST_BACKOFF", (0.0,))
    started, release = _gated_text_fallback(monkeypatch, adapter)
    card = AsyncMock(side_effect=[
        SendResult(success=False, error="Not connected"),  # the card
        SendResult(success=False, error="down"),  # repost 1: fails, registers a replacement view
        SendResult(success=True, message_id="3"),  # repost 2: shows the card on the replacement
    ])
    monkeypatch.setattr(adapter, "_send_exec_approval_card", card)
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    prompt_task = asyncio.create_task(adapter._send_exec_approval_prompt(_undelivered_prompt(session, request_id)))
    await asyncio.wait_for(started.wait(), 5)
    await asyncio.wait_for(adapter._repost_undelivered_exec_approval_cards(), 5)
    assert entry.undelivered is None
    await asyncio.wait_for(adapter._repost_undelivered_exec_approval_cards(), 5)
    assert card.await_count == 3
    assert entry.undelivered is None
    release.set()
    await asyncio.wait_for(prompt_task, 5)

    assert entry.undelivered is None
    await adapter.disconnect()


def _assert_withdrawn_plain(result, cause):
    assert result["approved"] is False
    assert result.get("user_consent") is False
    assert result["outcome"] == "cancelled"
    assert "denied by user" not in result["message"].lower()
    assert cause in result["message"]
    assert "NOT consented" in result["message"]


@pytest.fixture
def _gateway_approval_session(monkeypatch):
    """A gateway approval session in manual mode, as the tools-side attribution tests set up: the gate must
    prompt, so cron, yolo and interactive env are cleared and approval config is pinned to manual."""
    from tools import approval as mod
    from tools import approval_context
    key = "test-undelivered-repost-stop"
    mod._gateway_queues.clear()
    mod._gateway_notify_cbs.clear()
    mod._session_approved.clear()
    mod._denial_tally.pop(key, None)
    for k in ("HERMES_CRON_SESSION", "HERMES_YOLO_MODE", "HERMES_INTERACTIVE"):
        monkeypatch.delenv(k, raising=False)
    monkeypatch.setenv("HERMES_GATEWAY_SESSION", "1")
    monkeypatch.setenv("HERMES_SESSION_KEY", key)
    monkeypatch.setattr(mod, "_YOLO_MODE_FROZEN", False)
    monkeypatch.setattr(approval_context, "_get_approval_config", lambda: {"mode": "manual", "timeout": 60})
    yield key
    mod._gateway_queues.clear()
    mod._gateway_notify_cbs.clear()


@pytest.mark.asyncio
async def test_undelivered_approval_shown_by_a_later_repost_is_withdrawn_as_a_plain_stop(
        monkeypatch, _gateway_approval_session):
    """A card that failed, then was posted again on reconnect, was shown to Matt. A /stop after that repost keeps
    the withdrawal text; it must not say the approval was never shown."""
    import threading

    from gateway.platforms.base import ExecApprovalPrompt, SendResult
    from tools import approval as mod
    from tools.interrupt import set_interrupt

    session_key = _gateway_approval_session
    notified = threading.Event()
    seen = {}

    def notify(data):
        seen.update(data)
        notified.set()

    mod.register_gateway_notify(session_key, notify)
    holder = {}

    def worker():
        holder["tid"] = threading.get_ident()
        holder["result"] = mod.check_all_command_guards("rm -rf .git", "local")

    thread = threading.Thread(target=worker)
    thread.start()
    assert await asyncio.to_thread(notified.wait, 10), "approval was never enqueued"

    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token"))
    monkeypatch.setattr(adapter, "_EXEC_APPROVAL_REPOST_BACKOFF", (0.0,))
    monkeypatch.setattr(adapter, "_send_exec_approval_text_fallback", AsyncMock(return_value="send_path_degraded"))
    card = AsyncMock(side_effect=[
        SendResult(success=False, error="Not connected"),  # the first card never reaches Discord
        SendResult(success=True, message_id="2"),  # the reconnect repost does
    ])
    monkeypatch.setattr(adapter, "_send_exec_approval_card", card)
    prompt = ExecApprovalPrompt(
        chat_id="555", session_key=session_key, text="rm -rf .git", command="rm -rf .git", smart_denied=False,
        actions=[("Allow once", "once", "primary"), ("Deny", "deny", "danger")],
        request_id=seen["request_id"], metadata={"thread_id": "777"})

    await adapter._send_exec_approval_prompt(prompt)
    await asyncio.wait_for(adapter._repost_undelivered_exec_approval_cards(), 5)
    assert card.await_count == 2
    set_interrupt(True, holder["tid"], reason="Stop requested")
    try:
        await asyncio.to_thread(thread.join, 10)
    finally:
        set_interrupt(False, holder["tid"])
    assert not thread.is_alive()
    result = holder["result"]
    _assert_withdrawn_plain(result, "Stop requested")
    assert "approval was withdrawn before the user answered (Stop requested)" in result["message"]
    assert "never shown" not in result["message"]
    assert session_key not in mod._denial_tally


@pytest.mark.asyncio
async def test_in_flight_repost_whose_fallback_then_fails_is_named_never_shown(monkeypatch):
    # The card fails and its text fallback is in flight. A reconnect repost starts and its card send is still in
    # flight when the fallback fails. Neither surface has shown the prompt yet, so the request is marked never
    # shown. When the repost then fails too, the mark must stay: /stop must not read as a plain withdrawal.
    from gateway.platforms.base import SendResult

    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                            extra={"approval_card_confirm_timeout": 5}))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    monkeypatch.setattr(adapter, "_EXEC_APPROVAL_REPOST_BACKOFF", (0.0,))
    started, release = _gated_text_fallback(monkeypatch, adapter)
    card_calls = []
    repost_card_started, release_repost_card = asyncio.Event(), asyncio.Event()

    async def card(_prompt, _view):
        card_calls.append(1)
        if len(card_calls) == 1:
            return SendResult(success=False, error="Not connected")  # the card
        repost_card_started.set()
        await release_repost_card.wait()
        return SendResult(success=False, error="down")  # the repost
    monkeypatch.setattr(adapter, "_send_exec_approval_card", card)
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    prompt_task = asyncio.create_task(adapter._send_exec_approval_prompt(_undelivered_prompt(session, request_id)))
    await asyncio.wait_for(started.wait(), 5)
    repost_task = asyncio.create_task(adapter._repost_undelivered_exec_approval_cards())
    await asyncio.wait_for(repost_card_started.wait(), 5)
    release.set()
    await asyncio.wait_for(prompt_task, 5)
    assert entry.undelivered == "Not connected, then send_path_degraded"
    release_repost_card.set()
    await asyncio.wait_for(repost_task, 5)

    assert card_calls == [1, 1]
    assert entry.undelivered is not None
    assert entry.shown is False
    await adapter.disconnect()


@pytest.mark.asyncio
async def test_repost_that_times_out_leaves_the_request_never_shown(monkeypatch):
    # The card and its text fallback fail, so the request is marked never shown before any repost. The reconnect
    # repost's card send then hangs past the window. The card may or may not have reached Matt, so the repost
    # cannot prove it was shown: the never-shown mark must stay, and /stop must not read as a plain withdrawal.
    from gateway.platforms.base import SendResult

    adapter = DiscordAdapter(PlatformConfig(enabled=True, token="test-token",
                                            extra={"approval_card_confirm_timeout": 0.2}))
    created: list[_FakeBot] = []
    await _connect(adapter, monkeypatch, created)
    monkeypatch.setattr(adapter, "_EXEC_APPROVAL_REPOST_BACKOFF", (0.0,))
    monkeypatch.setattr(adapter, "_send_exec_approval_text_fallback", AsyncMock(return_value="send_path_degraded"))
    card_calls = []

    async def card(_prompt, _view):
        card_calls.append(1)
        if len(card_calls) == 1:
            return SendResult(success=False, error="Not connected")  # the card
        await asyncio.Event().wait()  # the repost hangs past the window
    monkeypatch.setattr(adapter, "_send_exec_approval_card", card)
    session = "agent:other:discord:555"
    request_id = uuid.uuid4().hex
    entry = _queue_entry(monkeypatch, session, request_id)

    await adapter._send_exec_approval_prompt(_undelivered_prompt(session, request_id))
    assert entry.undelivered == "Not connected, then send_path_degraded"
    # The card-failure withdrawal shares the window and would fire mid-repost. This test pins the repost timeout.
    adapter._exec_approval_views[request_id].undelivered_withdrawal.cancel()
    await asyncio.wait_for(adapter._repost_undelivered_exec_approval_cards(), 5)

    assert card_calls == [1, 1]
    assert entry.undelivered is not None
    assert entry.shown is False
    await adapter.disconnect()
