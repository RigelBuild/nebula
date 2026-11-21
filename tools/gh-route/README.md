# gh-route

Routes GitHub pull-request reads through REST or GraphQL, using the API bucket with more remaining capacity. It prints a REST-shaped result for either route, so agents can use it as a fallback when the review-state service is unavailable.

## Usage

```bash
cd tools/gh-route && go run . <cmd> [args]
```

Commands:

```text
gh-route pick
gh-route remaining <rest|graphql>
gh-route reviews <pr> [owner/repo]
gh-route comments <pr> [owner/repo]
gh-route review-comments <pr> [owner/repo]
gh-route check-runs <ref> [owner/repo]
gh-route head-sha <pr> [owner/repo]
gh-route pr-list [owner/repo]
```

Build from the module directory with `go build .`, or use the released `gh-route` package from the nebula release.

## Credentials

| Variable | Required | Source |
| --- | --- | --- |
| GitHub CLI authentication | yes | `gh` resolves authentication from its credential store or the caller's `GH_TOKEN`. The fleet normally uses `gh auth login`. |
| `GH_ROUTE_FLOOR`, `GH_ROUTE_CACHE_TTL`, `GH_ROUTE_MAX_WAIT` | no | Non-secret tuning values; defaults are 200, 15 seconds, and 300 seconds. |
| `GH_ROUTE_CACHE` | no | Non-secret path override; the default cache path is per-user under `TMPDIR` or `/tmp`. |

## Rotation

This tool does not store or mint a GitHub token. Refresh the GitHub CLI login with `gh auth login`, or update the caller's `GH_TOKEN`; GitHub issues the credential outside this repository. The same login is used by other `gh` commands in that user account.
