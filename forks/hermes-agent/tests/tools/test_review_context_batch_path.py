"""The reviewer sees the session's recent turns when a plugin escalation runs through a real tool batch.

Drives ``execute_tool_calls_sequential`` with a plugin ``pre_tool_call`` that asks for review, then reads
the prompt the reviewer model received. The user's go-ahead in this session must reach that prompt.
"""

import tempfile
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import MagicMock, patch

import pytest

from agent import tool_executor
from tools import approval


SESSION_GO = "yes, go ahead and run the vault command"


def _response(answer="APPROVE\nReads a file."):
    resp = MagicMock()
    resp.choices = [MagicMock()]
    resp.choices[0].message.content = answer
    resp.choices[0].finish_reason = "stop"
    return resp


def _tool_call(name="terminal", args='{"command": "echo hi"}', call_id="call-1"):
    tc = SimpleNamespace(id=call_id, type="function")
    tc.function = SimpleNamespace(name=name, arguments=args)
    return tc


@pytest.fixture
def batch_agent():
    from run_agent import AIAgent
    hermes_home = Path(tempfile.mkdtemp(prefix="hermes-test-home-"))
    (hermes_home / "logs").mkdir(parents=True, exist_ok=True)
    with (
        patch("model_tools.get_tool_definitions", return_value=[]),
        patch("model_tools.check_toolset_requirements", return_value={}),
        patch("agent.process_bootstrap.OpenAI"),
        patch("run_agent._hermes_home", hermes_home),
        patch("agent.model_metadata.fetch_model_metadata", return_value={}),
    ):
        agent = AIAgent(
            api_key="test-key", base_url="https://openrouter.ai/api/v1",
            quiet_mode=True, skip_context_files=True, skip_memory=True,
        )
    agent.client = MagicMock()
    agent.compression_enabled = False
    agent.save_trajectories = False
    return agent


def test_plugin_escalation_reviewer_prompt_has_the_users_go_ahead(batch_agent, monkeypatch):
    """A plugin escalation inside a real batch: the reviewer's prompt holds the user's turn."""
    seen = {}

    def _llm(**kwargs):
        seen["user"] = kwargs["messages"][1]["content"]
        return _response()

    monkeypatch.setattr("agent.auxiliary_client.call_llm", _llm)
    monkeypatch.setattr("tools.approval_context._get_approval_config", lambda: {"smart_policy": ""})
    monkeypatch.delenv("HERMES_APPROVAL_POLICY_FILE", raising=False)
    monkeypatch.setattr(approval, "_human_decision", lambda *_a, **_k: {"approved": True, "message": None})
    monkeypatch.setattr(approval, "_presence", lambda cb=None: (None, False, True, False))
    monkeypatch.setattr(approval, "is_approved", lambda *_a: False)
    monkeypatch.setattr("tools.approval_context._get_approval_mode", lambda: "manual")

    directive = {"action": "approve", "message": "vault run", "rule_key": "vault_run",
                 "display": "vault_secret run", "review": True}
    messages = [
        {"role": "user", "content": SESSION_GO},
        {"role": "assistant", "content": "I will run it now."},
    ]
    assistant = SimpleNamespace(tool_calls=[_tool_call()])
    with patch("hermes_cli.lifecycle.invoke_hook",
               side_effect=lambda name, **kw: [directive] if name == "pre_tool_call" else []), \
            patch("model_tools.handle_function_call", return_value='{"success": true}'):
        tool_executor.execute_tool_calls_sequential(batch_agent, assistant, messages, "task-1", finalize=False)

    assert "user" in seen, "the reviewer model was never called"
    assert SESSION_GO in seen["user"]
