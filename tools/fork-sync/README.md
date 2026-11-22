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

For a fork without a shared Git history, pass the upstream base explicitly with `--base <sha>`. Sync reads `GITHUB_TOKEN` for pull requests. Sync and export push through the Git credential helper configured in the environment. `GITHUB_REPOSITORY` and `GITHUB_API_URL` select the GitHub repository and API.

Each `upstream.json` `repo`, the `--upstream` value and the `--fork` URL must start with `https://`, `ssh://` or `git@`. A `RigelBuild/<repo>` fork maps to `https://` first. Any other value is refused before Git runs, so a value that starts with `-` cannot be read as a Git option. `--outbox` must not start with `-`.

Both a dry-run and a sync with no upstream changes print one plan line per fork. A no-op says the fork is up to date; an update dry-run reports the sync branch, upstream commit count, and commit range.

`.github/workflows/fork-sync.yaml` runs `sync --name <fork>` hourly, one job per `forks/<name>/upstream.json`, and on `workflow_dispatch` with an optional fork name. Each job mints a `rigelbuild-automation` App token and passes it as `GITHUB_TOKEN`, so the pull requests start CI. Each sync job fails while `vars.AUTOMATION_APP_CLIENT_ID` is unset in `main`, so a missing App cannot pass as a sync. The `export` command works. Only its scheduled run is not wired yet; it is planned for a later pull request. Export reads the outbox URL from `--outbox` and pushes the `upstream/<topic>` branch there; without it, export uses `RigelBuild/<name>`.
