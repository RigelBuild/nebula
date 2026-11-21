# depsupdate-wake

Checks whether the deps agent needs a daily all-mode run or a security wake. A user timer runs it every 2 hours.

## Usage

```sh
go run ./tools/depsupdate-wake
```

The Nix hosts run the packaged `depsupdate-wake` binary.

## Environment

| Variable | Source |
| --- | --- |
| `DEPSUPDATE_CLONE` | Path to the wake service's plain clone of `rigel.git`. |
| `DEPSUPDATE_STATE_DIR` | Directory containing `depsupdate.inflight` and `depsupdate.last-all`. |
| `COTAL_TO` | Cotal seat name to receive a wake DM. |
| `COTAL_DIR` | Working directory for the `cotal send` command. |

The wake sends an `all` DM when no all-mode landing has been recorded in the last 24 hours. Otherwise, a red vuln-gate sends a `security` DM with its last 20 output lines.
