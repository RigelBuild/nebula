# rjc-gates-gc

Removes expired runner worktrees and bounds the host caches under a runner
state root. It is a daily host job, so it runs without an interactive user.

## Usage

```sh
rjc-gates-gc --state <dir>
```

## Required inputs

| Input | Flag | Rule |
| --- | --- | --- |
| Runner state root | `--state` | Required. The command refuses to run without it and touches no files. |

There is no default state root. A deployment passes the root it uses.

## What it does

- Removes worktrees under `<state>/work` at contract depth
  (`<owner>/<repo>/<lane>/<workspace>`) that are older than 14 days and not
  locked by a running gate.
- Removes the oldest unlocked worktrees while the total is above 150 GiB.
- Empties `<state>/cache/go-build` when it is above 40 GiB and no worktree is
  locked.
- Empties `<state>/cache/go-mod` and `<state>/cache/bun` once a week, deferring
  while any worktree is locked.

## Example

```sh
rjc-gates-gc --state /srv/runner-state
```
