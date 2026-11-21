"""The plain-text approval prompt states the deadline the request really has.

Only Discord's own fallback withdraws an undelivered card early, after ``approval_card_confirm_timeout``,
and it passes that window as ``deadline_s``. Every other path waits ``approvals.timeout``, so its text
must keep saying that. Both prompt call sites are pinned: ``_format_exec_approval_fallback`` (an adapter
that splits long messages) and ``_cutting_text_prompt`` (an adapter that cuts them).
"""

from types import SimpleNamespace

from gateway.run_turn_runner_approval_text import approval_text_prompt
from tools import approval_context


def _splitting_adapter():
    return SimpleNamespace(splits_long_messages=True)


def _cutting_adapter():
    return SimpleNamespace(
        splits_long_messages=False,
        max_message_length_for_chat=lambda _c: 4096,
        message_len_fn_for_chat=lambda _c: len,
    )


def _prompt(adapter, **kwargs):
    return approval_text_prompt(
        adapter, "555", "agent:x:discord:555", "r1", "rm x", "scary", "/", **kwargs
    )


def test_fallback_states_approvals_timeout_when_the_request_is_not_withdrawn_early(
    monkeypatch,
):
    monkeypatch.setattr(approval_context, "_get_approval_timeout", lambda: 3600)
    assert "within 1 hour it will NOT run" in _prompt(_splitting_adapter())


def test_cutting_prompt_states_approvals_timeout_when_the_request_is_not_withdrawn_early(
    monkeypatch,
):
    monkeypatch.setattr(approval_context, "_get_approval_timeout", lambda: 3600)
    assert "within 1 hour it will NOT run" in _prompt(_cutting_adapter())


def test_explicit_withdrawal_window_is_stated_at_both_call_sites(monkeypatch):
    monkeypatch.setattr(approval_context, "_get_approval_timeout", lambda: 3600)
    assert "within 30 seconds it will NOT run" in _prompt(
        _splitting_adapter(), deadline_s=30
    )
    assert "within 30 seconds it will NOT run" in _prompt(
        _cutting_adapter(), deadline_s=30
    )
