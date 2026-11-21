# ci-queue

Lists Woodpecker queue tasks for a branch or pipeline and shows each pending task's dispatch rank. It is a local diagnostic CLI for humans and agents.

## Usage

```bash
go run . [--branch <name>] [--pipeline <number>] [--repo <owner/name>]
```

The repo is required: pass `--repo` or set `WOODPECKER_REPO`. The branch defaults to `WOODPECKER_BRANCH`, then the current Git branch.

## Credentials

| Variable | Required | Source |
| --- | --- | --- |
| `WOODPECKER_TOKEN`, `WOODPECKER_TOKEN_FILE` | yes | File value wins when non-empty after trimming. Use the environment variable or the file path. |
| `WOODPECKER_SERVER` | yes | Base URL of the Woodpecker server. |
| `WOODPECKER_REPO` | yes unless `--repo` is given | Repository full name, such as `example-owner/example-repo`. |
| `WOODPECKER_BRANCH` | no | Optional branch override; otherwise the current Git branch is used. |

## Rotation

Rotate the Woodpecker API token in the server, then update the value wherever your environment reads it. A running shell keeps the old value until it restarts, unless it reads the token from a file path.
