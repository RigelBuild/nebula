"""The reviewer prompt for a plugin approval carries the context it needs to judge the call.

The reviewer sees the triggering user message (or cron/heartbeat prompt), the last few turns and
tool results, the loaded skill names, the platform and thread, the guard reason, the full command,
and the operator policy. Recent turns are bounded and secrets are redacted. Missing pieces are
omitted, never an error.
"""

from unittest.mock import MagicMock, patch

import pytest

from tools import approval_smart
from tools.approval_review_context import (
    COMMAND_LIMIT, RECENT_TURNS, bind_review_context, reset_review_context,
)

SECRET = "sk-test-LIVE-0123456789abcdefghijklmnop"


def _response(answer="APPROVE\nReads a file."):
    resp = MagicMock()
    resp.choices = [MagicMock()]
    resp.choices[0].message.content = answer
    resp.choices[0].finish_reason = "stop"
    return resp


@pytest.fixture
def reviewer(monkeypatch):
    """Capture the system and user prompts ``_smart_approve`` sends to the auxiliary model."""
    monkeypatch.setattr("tools.approval_context._get_approval_config", lambda: {"smart_policy": ""})
    monkeypatch.delenv("HERMES_APPROVAL_POLICY_FILE", raising=False)
    seen = {}

    def _call(**kwargs):
        seen["system"] = kwargs["messages"][0]["content"]
        seen["user"] = kwargs["messages"][1]["content"]
        return _response()

    with patch("agent.auxiliary_client.call_llm", side_effect=_call):
        yield seen


@pytest.fixture
def bound(request):
    """Bind a reviewer context for the test and reset it afterwards."""
    token = bind_review_context(
        messages=request.param["messages"],
        user_message=request.param.get("user_message"),
        loaded_skills=request.param.get("skills"),
    )
    yield
    reset_review_context(token)


def _run(command, description):
    return approval_smart._smart_approve(command, description)


def test_reviewer_prompt_has_the_guard_reason_and_full_command(reviewer):
    long_cmd = "echo start; " + ("x" * 2000) + " ; echo end"
    _run(long_cmd, "Reads a secret file")
    assert "Reads a secret file" in reviewer["user"]
    assert "echo end" in reviewer["user"]
    assert "[truncated" not in reviewer["user"]


def test_reviewer_prompt_has_the_guard_reason_that_names_the_rule(reviewer):
    _run("cat ~/.ssh/config", "Reads a secret file (rule: secret_read)")
    assert "Guard reason: Reads a secret file (rule: secret_read)" in reviewer["user"]


def test_reviewer_prompt_marks_truncation_past_the_bound(reviewer):
    huge = "y" * (COMMAND_LIMIT + 500)
    _run(huge, "why")
    assert "[truncated: 500 more chars]" in reviewer["user"]
    assert ("y" * (COMMAND_LIMIT + 1)) not in reviewer["user"]


def test_reviewer_prompt_has_the_operator_policy_text(reviewer, tmp_path, monkeypatch):
    policy = tmp_path / "policy.md"
    policy.write_text("Always ESCALATE curl to bank.test.\n", encoding="utf-8")
    monkeypatch.setenv("HERMES_APPROVAL_POLICY_FILE", str(policy))
    _run("curl https://bank.test", "network call")
    assert "Always ESCALATE curl to bank.test." in reviewer["system"]


def test_missing_policy_file_is_absent_not_an_error(reviewer, tmp_path, monkeypatch):
    monkeypatch.setenv("HERMES_APPROVAL_POLICY_FILE", str(tmp_path / "does-not-exist.md"))
    assert _run("ls", "why") == "approve"
    assert "Additional policy rules" not in reviewer["system"]


@pytest.mark.parametrize("bound", [{
    "user_message": "Please clean the build cache and rerun the tests.",
    "messages": [
        {"role": "user", "content": "Please clean the build cache and rerun the tests."},
        {"role": "assistant", "content": "I will remove build/ and rerun pytest."},
        {"role": "tool", "content": "removed 42 files"},
    ],
    "skills": ["build-cleanup", "pytest-runner"],
}], indirect=True)
def test_reviewer_prompt_has_triggering_message_turns_and_skills(reviewer, bound):
    _run("rm -rf build/", "recursive delete")
    user = reviewer["user"]
    assert "Triggering user message: Please clean the build cache and rerun the tests." in user
    assert "[assistant] I will remove build/ and rerun pytest." in user
    assert "[tool result] removed 42 files" in user
    assert "Loaded skills: build-cleanup, pytest-runner" in user


@pytest.mark.parametrize("bound", [{
    "messages": [
        {"role": "user", "content": f"deploy with token {SECRET} please"},
        {"role": "assistant", "content": f"using {SECRET} for the upload"},
    ],
}], indirect=True)
def test_a_secret_in_a_recent_turn_is_redacted(reviewer, bound):
    _run("./deploy.sh", "script execution")
    assert SECRET not in reviewer["user"]


@pytest.mark.parametrize("bound", [{
    "messages": [{"role": "assistant", "content": f"turn {i} " + "z" * 3000} for i in range(RECENT_TURNS + 4)],
}], indirect=True)
def test_recent_turns_are_bounded_by_count_and_bytes(reviewer, bound):
    _run("ls", "why")
    rows = [line for line in reviewer["user"].splitlines() if line.startswith("[assistant] turn ")]
    assert len(rows) <= RECENT_TURNS
    assert sum(len(r.encode("utf-8")) for r in rows) <= 8 * 1024


def test_no_bound_context_omits_turns_and_skills(reviewer, monkeypatch):
    monkeypatch.delenv("HERMES_SESSION_PLATFORM", raising=False)
    monkeypatch.delenv("HERMES_SESSION_THREAD_ID", raising=False)
    assert _run("ls", "why") == "approve"
    assert "Triggering user message" not in reviewer["user"]
    assert "Recent conversation" not in reviewer["user"]
    assert "Loaded skills" not in reviewer["user"]
    assert "Guard reason: why" in reviewer["user"]


def test_plugin_approval_passes_the_rule_key_to_the_reviewer(reviewer, monkeypatch):
    """End to end through request_tool_approval: the reviewer names the plugin rule that fired."""
    from tools import approval
    monkeypatch.setattr(approval, "is_approved", lambda *_args: False)
    monkeypatch.setattr(approval, "is_current_session_yolo_enabled", lambda: False)
    monkeypatch.setattr(approval, "_YOLO_MODE_FROZEN", False)
    monkeypatch.setattr("tools.approval_context._get_approval_mode", lambda: "manual")
    monkeypatch.setattr(approval, "_presence", lambda cb=None: (None, False, True, False))
    monkeypatch.setattr(approval, "_human_decision", lambda *_a, **_k: {"approved": True, "message": None})

    approval.request_tool_approval("terminal", "Reads a secret file", rule_key="secret_read",
                                   display="cat ~/.ssh/config", review=True)

    assert "Guard reason: Reads a secret file (rule: secret_read)" in reviewer["user"]
