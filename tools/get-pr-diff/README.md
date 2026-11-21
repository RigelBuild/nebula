# get-pr-diff

Fetches a pull request's unified diff and complete touched-file list from GitHub or Forgejo. The review agent consumes the same JSON output for either forge.

## Usage

```sh
GOWORK=off go run ./tools/get-pr-diff <pr> [owner/repo | --repo owner/repo] [--forge github|forgejo]
```

The review loop invokes it when an agent reviews a PR. Run the Go binary from the repository root. GitHub is the default; Forgejo requires `--forge forgejo` and an explicit repository.

## Credentials

| Variable | Required | Source |
| --- | --- | --- |
| `gh` login or `GH_TOKEN` | yes for GitHub | Ambient `gh` authentication or a caller-supplied `GH_TOKEN`. |
| `FORGEJO_BASE_URL` | yes for Forgejo | Non-secret API endpoint supplied by the caller. |
| `FORGEJO_TOKEN` | no for public Forgejo repos; yes for private repos | Optional Forgejo API token supplied by the caller. Its issuer and source are not identified in this repo. |

## Rotation

Caller-supplied; rotate at the source.
