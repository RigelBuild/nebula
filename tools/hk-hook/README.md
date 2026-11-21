# hk-hook

Installs or updates the repository's hk pre-push hook. The hook enters the devenv through direnv, then runs the local `moon ci` gate so pushes use the same checks as the development shell.

## Usage

```bash
go run ./tools/hk-hook
```

Run it from the checkout, or a jj secondary workspace, whose hook you want to install. It takes no arguments.

A devenv shell hook runs the installed `hk-hook` binary whenever a developer enters the shell. It writes local Git configuration for `hk-pre-push`; the hook runs `direnv exec . hk run pre-push --from-hook` on a push. `HK=0` skips the hook.

The devenv sets `HK_STATE_DIR` to `.devenv/state/hk`, so each workspace's failed push writes `output.log` there, not to the shared `~/.local/state/hk/`.

The local gate limits Moon to four concurrent tasks. Concurrent Nix evaluations can exhaust the agent session's memory cap and get killed without a Nix error. This limit does not change remote CI scheduling or skip checks.

## Credentials

None.
