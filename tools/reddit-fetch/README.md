# reddit-fetch

Fetches Reddit's unauthenticated `.json` endpoints for manual sentiment reads in model-selection vetting. It streams response bodies in target order and stops on a failed request.

## Usage

```bash
cd tools/reddit-fetch && go run . <target> [<target> ...] [--limit N] [--delay-ms N] [--max-retries N] [--user-agent UA]
```

For example, `cd tools/reddit-fetch && go run . 'r/LocalLLaMA/top?t=week' --limit 10`. A human or agent runs it when preparing a rubric read; it is not a CI job. The default delay is 2000 ms between targets and the default retry count is 3 for HTTP 429 responses. The exit code is 0 when every target is fetched, 1 on any fetch failure, and 2 on bad arguments.

## Credentials

| Variable | Required | Source |
| --- | --- | --- |
| `REDDIT_USER_AGENT` | no | Optional request User-Agent override, used when `--user-agent` is not given. |

No credentials are read. Reddit access is unauthenticated.
