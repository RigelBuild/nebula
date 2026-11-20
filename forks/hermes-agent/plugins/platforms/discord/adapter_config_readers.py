"""config.yaml readers for the Discord adapter: DM role-auth guild opt-in, button-view timeout and
exec-approval admin gate.

Re-exported from ``plugins.platforms.discord.adapter`` (callers and tests import them there).
"""
from typing import Any, Optional

from agent.deadline import MAX_SAFE_TIMEOUT_S


def _read_dm_role_auth_guild() -> Optional[int]:
    """Return the guild ID opted-in for DM role-based auth, or None (secure default). Read from
    config.yaml ``discord.dm_role_auth_guild`` only (behavioral, not a secret); int or numeric string."""
    try:
        from hermes_cli.config import read_raw_config
        cfg = read_raw_config() or {}
        discord_cfg = cfg.get("discord", {}) or {}
        raw = discord_cfg.get("dm_role_auth_guild")
    except Exception:
        return None
    if raw is None or raw == "":
        return None
    try:
        guild_id = int(raw)
    except (TypeError, ValueError):
        return None
    return guild_id if guild_id > 0 else None


# All Discord prompt views share this setting; command approval waits have their own timeout.
_DISCORD_PROMPT_TIMEOUT_DEFAULT = 300
_DISCORD_PROMPT_TIMEOUT_MIN = 30
_DISCORD_PROMPT_TIMEOUT_MAX = int(MAX_SAFE_TIMEOUT_S)


def _read_discord_prompt_timeout() -> int:
    """Timeout (seconds) for Discord button views from ``approvals.discord_prompt_timeout``
    (default 300), clamped to [MIN, MAX] to keep their UI lifetime bounded."""
    raw: Any = None
    try:
        from hermes_cli.config import read_raw_config
        cfg = read_raw_config() or {}
        approvals_cfg = cfg.get("approvals", {}) or {}
        raw = approvals_cfg.get("discord_prompt_timeout")
    except Exception:
        return _DISCORD_PROMPT_TIMEOUT_DEFAULT
    if raw is None or raw == "":
        return _DISCORD_PROMPT_TIMEOUT_DEFAULT
    try:
        seconds = int(raw)
    except (TypeError, ValueError):
        return _DISCORD_PROMPT_TIMEOUT_DEFAULT
    if seconds < _DISCORD_PROMPT_TIMEOUT_MIN:
        return _DISCORD_PROMPT_TIMEOUT_MIN
    if seconds > _DISCORD_PROMPT_TIMEOUT_MAX:
        return _DISCORD_PROMPT_TIMEOUT_MAX
    return seconds


def _read_discord_prompt_timeout_within(backing_seconds: float) -> int:
    """Prompt timeout capped at the backing wait's lifetime, so a card never outlives the request
    it answers. ``backing_seconds <= 0`` means the backing wait is unbounded."""
    seconds = _read_discord_prompt_timeout()
    if backing_seconds > 0:
        return min(seconds, max(int(backing_seconds), 1))
    return seconds


def _resolve_exec_approval_admin_gate(config_extra: Optional[dict]) -> tuple[bool, set]:
    """Resolve the exec-approval admin gate from ``extra``; returns ``(require_admin, admin_user_ids)``.
    Default OFF (user-scope buttons). When ``require_admin_for_exec_approval`` is true only
    ``allow_admin_from`` ids may click; on with no admins -> ``(True, set())`` (fail closed, log once).
    """
    extra = config_extra if isinstance(config_extra, dict) else {}
    raw_toggle = extra.get("require_admin_for_exec_approval", False)
    require_admin = str(raw_toggle).strip().lower() in {"true", "1", "yes"}
    if not require_admin:
        return (False, set())
    try:
        from gateway.slash_access import _coerce_id_list
        admin_ids = set(_coerce_id_list(extra.get("allow_admin_from")))
    except Exception:
        admin_ids = set()
    return (True, admin_ids)


_APPROVAL_CARD_CONFIRM_TIMEOUT_DEFAULT = 30


def _read_approval_card_confirm_timeout(config_extra: Optional[dict]) -> float:
    """Seconds from ``extra["approval_card_confirm_timeout"]`` (default 30): how long a card send may take to
    confirm, and how long the plain-text fallback then waits for an answer before the request is withdrawn."""
    extra = config_extra if isinstance(config_extra, dict) else {}
    try:
        seconds = float(extra.get("approval_card_confirm_timeout", _APPROVAL_CARD_CONFIRM_TIMEOUT_DEFAULT))
    except (TypeError, ValueError):
        return _APPROVAL_CARD_CONFIRM_TIMEOUT_DEFAULT
    return seconds if seconds > 0 else _APPROVAL_CARD_CONFIRM_TIMEOUT_DEFAULT


def _read_approval_text_approver_id(config_extra: Optional[dict]) -> str:
    """The one user id whose typed ``approve`` / ``deny`` in an approval's thread answers it. Empty (the default)
    disables text answers: fail closed."""
    extra = config_extra if isinstance(config_extra, dict) else {}
    return str(extra.get("approval_text_approver_id") or "").strip()
