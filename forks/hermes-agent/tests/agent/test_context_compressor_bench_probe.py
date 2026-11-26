"""The summary-model bench (fallback streak >= 2) and its persisted probe window."""

from unittest.mock import patch

from agent.context_compressor import ContextCompressor
from hermes_state import SessionDB


def _compressor():
    with patch("agent.context_compressor.get_model_context_length", return_value=100000):
        c = ContextCompressor(model="test/model", threshold_percent=0.85, protect_first_n=2, protect_last_n=2, quiet_mode=True)
        _ = c.context_length
        return c


def _messages(n_pairs=10):
    msgs = [{"role": "system", "content": "system prompt"}]
    for i in range(n_pairs):
        msgs += [{"role": "user", "content": f"question {i}"}, {"role": "assistant", "content": "short reply"}]
    return msgs


def _finish_boundary(compressor):
    """Mirror the boundary wrapper's bookkeeping after a compaction (agent/conversation_compression.py)."""
    compressor.record_completed_compaction(
        used_fallback=compressor._last_summary_fallback_used,
        feasibility_skip=compressor._last_feasibility_skip,
    )


def test_bench_probe_deadline_survives_a_compressor_rebuild(tmp_path):
    """The gateway rebuilds the agent on cache eviction: a fresh compressor bound to the same
    session must resume the persisted bench window, not restart it (else the probe starves).
    A failed probe re-benches for a fresh window, and that window follows a rotating compaction."""
    db = SessionDB(db_path=tmp_path / "state.db")
    db.create_session("s1", "cli")
    db.set_compression_fallback_streak("s1", 2)
    db.patch_session_model_config("s1", {"_fallback_probe_at": 1300.0})
    compressor = _compressor()
    compressor.bind_session_state(db, "s1")

    with patch("agent.context_compressor.time.time", return_value=1000.0), \
            patch.object(compressor, "_generate_summary", return_value="LLM summary") as mock_gen:
        compressor.compress(_messages(), force=False)  # inside persisted window
    mock_gen.assert_not_called()

    rebuilt = _compressor()
    rebuilt.bind_session_state(db, "s1")
    with patch("agent.context_compressor.time.time", return_value=1301.0), \
            patch.object(rebuilt, "_generate_summary", return_value=None) as mock_gen:
        rebuilt.compress(_messages(), force=False)  # probe after the window fails
        _finish_boundary(rebuilt)
    mock_gen.assert_called_once()
    assert db.get_session_model_config_value("s1", "_fallback_probe_at") == 1601.0

    # Rotating compaction (in_place: false) binds a fresh child row: the re-armed deadline must follow it.
    with patch("agent.context_compressor.time.time", return_value=1400.0), \
            patch.object(rebuilt, "_generate_summary", return_value="LLM summary") as mock_gen:
        rebuilt.compress(_messages(), force=False)  # inside the re-armed window
    mock_gen.assert_not_called()
    db.create_session("s2", "cli", parent_session_id="s1")
    rebuilt.on_session_start("s2", boundary_reason="compression", old_session_id="s1", session_db=db)
    with patch("agent.context_compressor.time.time", return_value=1602.0), \
            patch.object(rebuilt, "_generate_summary", return_value="LLM summary") as mock_gen:
        rebuilt.compress(_messages(), force=False)
    mock_gen.assert_called_once()


def test_benched_summary_model_is_retried_after_window_and_success_resets_streak(tmp_path):
    """Two failed summaries bench the model. The first compaction after the recovery window probes it
    once, and a good summary clears the persisted streak so the bench does not stay armed."""
    db = SessionDB(db_path=tmp_path / "state.db")
    db.create_session("s1", "cli")
    compressor = _compressor()
    compressor.bind_session_state(db, "s1")
    window = compressor._ANTI_THRASH_RECOVERY_SECONDS

    with patch("agent.context_compressor.time.time", return_value=1000.0):
        for _ in range(2):
            with patch.object(compressor, "_generate_summary", return_value=None) as mock_gen:
                compressor.compress(_messages(), force=False)
            mock_gen.assert_called_once()
            _finish_boundary(compressor)
    assert db.get_compression_fallback_streak("s1") == 2
    assert db.get_session_model_config_value("s1", "_fallback_probe_at") == 1000.0 + window

    with patch("agent.context_compressor.time.time", return_value=1000.0 + window + 1):
        with patch.object(compressor, "_generate_summary", return_value="LLM summary") as mock_gen:
            compressor.compress(_messages(), force=False)
        mock_gen.assert_called_once()
        _finish_boundary(compressor)
    assert db.get_compression_fallback_streak("s1") == 0
    assert db.get_session_model_config_value("s1", "_fallback_probe_at") is None


def test_failed_probe_rebenches_for_another_window(tmp_path):
    """A probe that fails again must bench the model for a fresh recovery window."""
    db = SessionDB(db_path=tmp_path / "state.db")
    db.create_session("s1", "cli")
    db.set_compression_fallback_streak("s1", 2)
    compressor = _compressor()
    compressor.bind_session_state(db, "s1")
    window = compressor._ANTI_THRASH_RECOVERY_SECONDS

    with patch("agent.context_compressor.time.time", return_value=1000.0):
        with patch.object(compressor, "_generate_summary", return_value=None) as mock_gen:
            compressor.compress(_messages(), force=False)
        mock_gen.assert_called_once()
        _finish_boundary(compressor)
    assert db.get_compression_fallback_streak("s1") == 3
    assert db.get_session_model_config_value("s1", "_fallback_probe_at") == 1000.0 + window

    with patch("agent.context_compressor.time.time", return_value=1000.0 + window - 1), \
            patch.object(compressor, "_generate_summary", return_value="LLM summary") as mock_gen:
        compressor.compress(_messages(), force=False)
    mock_gen.assert_not_called()


def test_saved_streak_without_probe_deadline_probes_on_next_compression(tmp_path):
    """A streak saved before deadlines were recorded (streak 2, no timestamp) must not bench
    the model for another window: the next compaction probes it, and a good summary heals it."""
    db = SessionDB(db_path=tmp_path / "state.db")
    db.create_session("s1", "cli")
    db.set_compression_fallback_streak("s1", 2)
    compressor = _compressor()
    compressor.bind_session_state(db, "s1")

    with patch("agent.context_compressor.time.time", return_value=1000.0), \
            patch.object(compressor, "_generate_summary", return_value="LLM summary") as mock_gen:
        compressor.compress(_messages(), force=False)
        mock_gen.assert_called_once()
        _finish_boundary(compressor)
    assert db.get_compression_fallback_streak("s1") == 0


def test_loaded_streak_stays_benched_inside_persisted_window(tmp_path):
    """Inside the persisted window the model stays benched: no summary call, and the streak and
    deadline are left as they were."""
    db = SessionDB(db_path=tmp_path / "state.db")
    db.create_session("s1", "cli")
    db.set_compression_fallback_streak("s1", 2)
    db.patch_session_model_config("s1", {"_fallback_probe_at": 1300.0})
    compressor = _compressor()
    compressor.bind_session_state(db, "s1")

    with patch("agent.context_compressor.time.time", return_value=1299.0), \
            patch.object(compressor, "_generate_summary", return_value="LLM summary") as mock_gen:
        compressor.compress(_messages(), force=False)
        mock_gen.assert_not_called()
        _finish_boundary(compressor)
    assert db.get_compression_fallback_streak("s1") == 2
    assert db.get_session_model_config_value("s1", "_fallback_probe_at") == 1300.0


def test_aborted_probe_arms_deadline_and_prevents_same_window_retry(tmp_path):
    """A terminal failure aborts before boundary accounting, but the probe still consumes its window."""
    db = SessionDB(db_path=tmp_path / "state.db")
    db.create_session("s1", "cli")
    db.set_compression_fallback_streak("s1", 2)
    compressor = _compressor()
    compressor.bind_session_state(db, "s1")
    now = 1000.0
    window = compressor._ANTI_THRASH_RECOVERY_SECONDS
    messages = _messages()

    with patch("agent.context_compressor.time.time", return_value=now), \
            patch("agent.context_compressor.call_llm", side_effect=ConnectionError("Connection error.")), \
            patch.object(compressor, "_generate_summary", wraps=compressor._generate_summary) as mock_gen:
        result = compressor.compress(messages, force=False)
        assert compressor._last_compress_aborted is True
        assert result == messages
        mock_gen.assert_called_once()

        # Bypass only the unrelated failure cooldown so this second attempt tests the persisted bench.
        compressor._clear_compression_failure_cooldown()
        compressor.compress(messages, force=False)
        mock_gen.assert_called_once()

    assert db.get_session_model_config_value("s1", "_fallback_probe_at") == now + window
