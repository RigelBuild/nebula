"""Reset Hermes state after preserving the current SQLite files."""

from __future__ import annotations

from collections.abc import Callable


def build_state_parser(subparsers, *, cmd_state_reset: Callable) -> None:
    state_parser = subparsers.add_parser("state", help="Manage Hermes state")
    actions = state_parser.add_subparsers(dest="state_action")
    reset = actions.add_parser(
        "reset",
        help="Move the current session database aside and restart the gateway (one-shot per home)",
        description="Reset gateway state once per Hermes home; subsequent invocations do not reset again.",
    )
    reset.set_defaults(func=cmd_state_reset)


def cmd_state_reset(_args) -> int:
    """Stop the gateway, move its DB aside, then start it again."""
    from hermes_cli.state_reset import reset_state

    return reset_state()

