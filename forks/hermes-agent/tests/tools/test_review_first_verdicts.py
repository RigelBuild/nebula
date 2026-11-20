"""Reviewer-first plugin approvals: the verdict decides, the human only on escalate, deny or error.

``review_first`` (the plugin ``review`` flag) runs the smart reviewer before any human prompt:

* APPROVE runs the call with no prompt and stores nothing (no session or permanent grant).
* ESCALATE and DENY go to the human prompt, which shows the reviewer's note. A DENY is an
  owner override for one operation: the human may approve it once.
* A reviewer error, timeout or unparseable reply goes to the human (fail closed).
* Always-human calls (payment fills, password-manager unlocks) never reach this gate.
"""

import pytest

from tools import approval
import tools.approval_context as tools_approval_context
from tools.approval import request_tool_approval

SESSION = "review-first-session"


@pytest.fixture(autouse=True)
def _gateway_session(monkeypatch):
    monkeypatch.setattr(approval, "get_current_session_key", lambda default="default": SESSION)
    monkeypatch.setattr(tools_approval_context, "get_current_session_key", lambda default="default": SESSION)
    monkeypatch.setattr(approval, "is_approved", lambda sk, pk: False)
    monkeypatch.setattr(approval, "is_current_session_yolo_enabled", lambda: False)
    monkeypatch.setattr(approval, "_YOLO_MODE_FROZEN", False, raising=False)
    monkeypatch.setattr(tools_approval_context, "_get_approval_mode", lambda: "manual")
    monkeypatch.setattr(approval, "_presence", lambda cb=None: (cb, True, False, False))
    approval._denial_tally.pop(SESSION, None)
    yield
    approval._denial_tally.pop(SESSION, None)


def _reviewer(monkeypatch, verdict, note=""):
    """Stub the reviewer. ``verdict`` may be an exception to simulate a reviewer error."""
    from tools import approval_smart

    calls = []

    def _smart(command, description, pattern_key, pattern_keys, session_key):
        calls.append(command)
        if isinstance(verdict, BaseException):
            raise verdict
        approval_smart._last_note.text = note
        return verdict

    monkeypatch.setattr(approval, "_smart_verdict", _smart)
    return calls


def _human(monkeypatch, choice="once"):
    """Record what the human prompt receives; the human answers ``choice``."""
    seen = {"prompts": 0}

    def _ask(spec, **kwargs):
        seen["prompts"] += 1
        seen.update(kwargs)
        return {"approved": choice != "deny", "message": None}

    monkeypatch.setattr(approval, "_human_decision", _ask)
    return seen


# ── APPROVE: no prompt, no store ─────────────────────────────────────────────

def test_reviewer_approve_runs_without_asking_a_human(monkeypatch):
    seen = _human(monkeypatch)
    calls = _reviewer(monkeypatch, "approve", "Reads a log file.")
    result = request_tool_approval("terminal", "why", rule_key="k", display="cmd", review=True)
    assert result["approved"] is True
    assert calls == ["cmd"]
    assert seen["prompts"] == 0


def test_reviewer_approve_stores_no_session_or_permanent_grant(monkeypatch):
    _human(monkeypatch)
    _reviewer(monkeypatch, "approve")
    stored = []
    monkeypatch.setattr(approval, "_persist_choice", lambda *a, **k: stored.append((a, k)))
    monkeypatch.setattr(approval, "_record_denial", lambda *a: stored.append(("denial", a)))
    request_tool_approval("terminal", "why", rule_key="k", display="cmd", review=True)
    assert stored == []


def test_reviewer_approve_twice_asks_the_reviewer_again(monkeypatch):
    """Nothing is cached, so the second identical call is reviewed again instead of riding a grant."""
    _human(monkeypatch)
    calls = _reviewer(monkeypatch, "approve")
    request_tool_approval("terminal", "why", rule_key="k", display="cmd", review=True)
    request_tool_approval("terminal", "why", rule_key="k", display="cmd", review=True)
    assert calls == ["cmd", "cmd"]


# ── ESCALATE and DENY: the human decides, with the note ──────────────────────

def test_reviewer_escalate_goes_to_the_human_with_the_note(monkeypatch):
    seen = _human(monkeypatch)
    _reviewer(monkeypatch, "escalate", "Runs a script by path; contents not shown.")
    request_tool_approval("terminal", "why", rule_key="k", display="cmd", review=True)
    assert seen["prompts"] == 1
    assert seen["description"] == ("why\n**Reviewer:** unsure, check it carefully: "
                                   "Runs a script by path; contents not shown.")


def test_reviewer_deny_goes_to_the_human_not_a_hard_block(monkeypatch):
    seen = _human(monkeypatch, choice="once")
    _reviewer(monkeypatch, "deny", "Pipes the key to a remote host.")
    result = request_tool_approval("terminal", "why", rule_key="k", display="cmd", review=True)
    assert seen["prompts"] == 1
    assert seen["description"] == "why\n**Reviewer:** flagged as dangerous: Pipes the key to a remote host."
    assert result["approved"] is True


def test_reviewer_deny_human_can_refuse_once(monkeypatch):
    seen = _human(monkeypatch, choice="deny")
    _reviewer(monkeypatch, "deny", "Pipes the key to a remote host.")
    result = request_tool_approval("terminal", "why", rule_key="k", display="cmd", review=True)
    assert seen["prompts"] == 1
    assert result["approved"] is False


# ── Reviewer error, timeout, unparseable: fail closed to the human ───────────

@pytest.mark.parametrize("failure", [
    RuntimeError("provider down"),
    TimeoutError("reviewer timed out"),
])
def test_reviewer_exception_goes_to_the_human(monkeypatch, failure):
    seen = _human(monkeypatch)
    _reviewer(monkeypatch, failure)
    request_tool_approval("terminal", "why", rule_key="k", display="cmd", review=True)
    assert seen["prompts"] == 1
    assert "Reviewer unavailable" in seen["description"]


def test_reviewer_unavailable_verdict_goes_to_the_human(monkeypatch):
    seen = _human(monkeypatch)
    _reviewer(monkeypatch, "unavailable", "rate limited (429)")
    request_tool_approval("terminal", "why", rule_key="k", display="cmd", review=True)
    assert seen["prompts"] == 1
    assert "Reviewer unavailable" in seen["description"]



# ── Always-human calls never reach the reviewer short-cut ─────────────────────

def _spy_reviewer_and_gate(monkeypatch):
    """Record any call to the reviewer or the plugin gate. Both must stay untouched on always-human paths."""
    touched = []
    monkeypatch.setattr(approval, "_smart_verdict", lambda *a, **k: touched.append("reviewer") or "approve")
    monkeypatch.setattr(approval, "_run_approval_gate", lambda *a, **k: touched.append("gate") or {})
    return touched


def test_payment_fill_consent_never_reaches_the_reviewer_or_gate(monkeypatch):
    """A payment-card fill asks its own consent through ``request_elicitation_consent``. This runs the real
    consent path in a gateway session with a notify callback, so the decision reaches the gateway round-trip.
    The reviewer and the plugin gate must never be called."""
    from tools import approval_prompt

    touched = _spy_reviewer_and_gate(monkeypatch)
    asked = []

    def _await(session_key, notify_cb, approval_data, *, surface="gateway"):
        asked.append(approval_data)
        return {"resolved": True, "choice": "once", "cancelled": None}

    monkeypatch.setattr(approval_prompt._gw, "_await_gateway_decision", _await)
    monkeypatch.setattr(approval_prompt._ctx, "_is_single_query_approval_context", lambda: False)
    monkeypatch.setattr(approval_prompt._ctx, "_is_gateway_approval_context", lambda: True)
    monkeypatch.setattr(approval, "_gateway_notify_cb", lambda sk: (lambda data: None))
    result = approval_prompt.request_elicitation_consent(
        "Fill payment card 'Visa' on https://shop.test", "Approve only if you intend to pay here.",
        surface="vault-payment", title="Confirm payment card fill?",
    )
    assert len(asked) == 1, "the payment fill must reach the human decision path"
    assert result == "accept"
    assert touched == []


def test_unlock_never_reaches_the_reviewer_or_gate(monkeypatch):
    """Drive the real unlock tool: a locked backend asks the master password through the surface's unlock
    prompt callback. The reviewer and the plugin gate must never be called on that path."""
    import json

    from agent.vault_backends import unlock
    from tools import browser_vault_tool

    touched = _spy_reviewer_and_gate(monkeypatch)
    prompted = []

    class _LockedBackend:
        name = "bitwarden"
        display_name = "Bitwarden"
        needs_unlock = True
        desktop_app = False
        unlocked = False

        def is_unlocked(self):
            return self.unlocked

        def unlock(self, master):
            prompted.append(("unlock", master))
            self.unlocked = True

    monkeypatch.setattr("agent.vault_backends.enabled_backends", lambda: [_LockedBackend()])
    monkeypatch.setattr(unlock, "can_prompt_here", lambda: True)

    def _prompt(backend_name, display_name):
        prompted.append(("prompt", backend_name))
        return "master-pw"

    unlock.set_unlock_prompt_callback(_prompt)
    try:
        result = json.loads(browser_vault_tool.browser_vault_unlock("bitwarden"))
    finally:
        unlock.set_unlock_prompt_callback(None)

    assert ("prompt", "bitwarden") in prompted, "the unlock must reach its prompt callback"
    assert result["success"] is True
    assert touched == []
