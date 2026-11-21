# linear-comment-poller

Fetches comments for watched Linear issues into a shared cache, then wakes the owning agents when new comments arrive. A timer runs `sweep` on a schedule; `read` reports from the cache without calling Linear.

## Usage

```bash
linear-comment-poller --issue-prefix EX --issue-prefix DEMO sweep
linear-comment-poller --issue-prefix EX read --watchlist <path>
```

`--issue-prefix` is required and repeatable. The first prefix is the Linear team the sweep queries; every later prefix is an alias, so `DEMO-7` is read as `EX-7`. `LINEAR_ISSUE_PREFIXES` (comma-separated, same order) is used when no flag is given. There is no default.

## Environment

| Variable | Required | Purpose |
| --- | --- | --- |
| `LINEAR_ISSUE_PREFIXES` | unless `--issue-prefix` is set | Comma-separated team keys; the first is the team, the rest are aliases. |
| `LINEAR_CLIENT_ID_FILE` | yes for a non-empty watchlist | File holding the poller's Linear OAuth client id. |
| `LINEAR_CLIENT_SECRET_FILE` | yes for a non-empty watchlist | File holding the poller's Linear OAuth client secret. |
| `LINEAR_OWN_USER_ID` | yes for a non-empty watchlist | Linear user id of the agents' own account. Its comments do not wake anyone. |
| `LINEAR_AGENT_USER_ID_FILE` | no | File holding the Linear app user's id. Its comments do not wake anyone. |
| `WAVE_WORKSPACES`, `LINEAR_COMMENTS_CACHE`, `LINEAR_COMMENTS_NOTIFY` | no | Path overrides; defaults are `~/agents/workspaces` and files under `~/agents/state`. |

## Rotation

Re-issue the client secret on the poller's Linear OAuth app and replace the files the two `*_FILE` variables point to. Use an OAuth app dedicated to the poller. Each sweep revokes only the token it minted.
