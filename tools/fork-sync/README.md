# fork-sync

The `fork-sync` command imports upstream fork trees into `forks/`, syncs recorded upstream commits into one `sync/<name>` branch and pull request, and exports nebula commit ranges to outbox branches.

## Commands

```sh
go run . import --name jj-vine --fork RigelBuild/jj-vine --ref rigel-release \
  --upstream https://codeberg.org/abrenneke/jj-vine.git --upstream-branch main

go run . sync --name jj-vine --dry-run
go run . sync --all
go run . export --name jj-vine --range <start>..<end> --topic <topic>
```

For a fork without a shared Git history, pass the upstream base explicitly with `--base <sha>`. Sync and export require the relevant branch credentials in the Git environment. Sync PRs use `GITHUB_TOKEN`; a private GitHub outbox push uses its credential helper or a token configured by the workflow.

Both a dry-run and a sync with no upstream changes print one plan line per fork. A no-op says the fork is up to date; an update dry-run reports the sync branch, upstream commit count, and commit range.

The scheduled workflow uses the GitHub App credentials `NEBULA_APP_CLIENT_ID` and `NEBULA_APP_PRIVATE_KEY` from the `main` environment. It creates a short-lived token for the nebula repository, then creates or updates the matching pull request. Export accepts an explicit outbox URL for local repositories; otherwise it uses `RigelBuild/<name>`.
