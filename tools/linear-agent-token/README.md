# linear-agent-token

Mints a Linear access token for an OAuth app using client credentials (actor = app), and writes the token and the app user id to files.

## Usage

```bash
go run .
```

## Environment

| Variable | Source |
| --- | --- |
| `LINEAR_CLIENT_ID_FILE`, `LINEAR_CLIENT_SECRET_FILE` | Paths to files holding the app's client id and client secret. |
| `LINEAR_AGENT_TOKEN_FILE` | Output: the access token. |
| `LINEAR_AGENT_USER_ID_FILE` | Output: the app user's id. |

Both outputs are replaced atomically with mode `0600`. A failed mint or viewer check leaves the previous files in place.

## Token lifetime

Tokens last 30 days, and Linear allows many live tokens with the same scope, so each mint leaves the older token valid. The scope is fixed at `read,write`. Minting with a different scope revokes every token of the app, so never reuse one app for another consumer.

## Rotation

Re-issue the client secret on the OAuth app, update the file behind `LINEAR_CLIENT_SECRET_FILE`, then run the tool again.
