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

For a fork without a shared Git history, pass the upstream base explicitly with `--base <sha>`. Sync and export read `GITHUB_TOKEN` for pull requests, and push through the Git credential helper configured in the environment. `GITHUB_REPOSITORY` and `GITHUB_API_URL` select the GitHub repository and API.

Each `upstream.json` `repo` and the `--upstream` value must start with `https://`, `ssh://` or `git@`. Any other value is refused before Git runs, so a repo that starts with `-` cannot be read as a Git option.

Both a dry-run and a sync with no upstream changes print one plan line per fork. A no-op says the fork is up to date; an update dry-run reports the sync branch, upstream commit count, and commit range.

No GitHub workflow runs this command yet. Scheduled sync and outbox export are planned for a later pull request. Export writes the outbox URL to `--outbox`; without it, export uses `RigelBuild/<name>`.
