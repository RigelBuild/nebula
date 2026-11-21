# nemo-move

`nemo-move` provides the k3s owner gate, gateway probes, cold transfer and verification commands, and SSH operations for stopping and exporting Nemo state from a macOS source host.

## Commands

```text
nemo-move gate [--phase /control/phase] [--wait]
nemo-move probe startup|live|ready [--phase /control/phase]
nemo-move seal --dir /control/nemo-page
nemo-move verify --as <user> --root <path> --manifest <path>
nemo-move inventory --mac <user>@<host> --home-root <home> --ssh-user <user> --mac-binary ./nemo-move-darwin
nemo-move mac stop --mac <user>@<host> --home-root <home> --ssh-user <user> --launchd-label <label>:<gui|system> [--launchd-label ...]
nemo-move mac export --mac <user>@<host> --home-root <home> --ssh-user <user> --mac-binary ./nemo-move-darwin
nemo-move cutover --kubeconfig <path> --mac <user>@<host> --home-root <home> --ssh-user <user> --launchd-label <label>:<gui|system> [--launchd-label ...] --mac-binary ./nemo-move-darwin
nemo-move restore --kubeconfig <path> --backup-data <name> --backup-control <name> --node-confirmed-off <node>
```

`--home-root` is the absolute home directory of the source account, for example `/Users/example`. `--ssh-user` is that account's SSH user, and must match the user in `--mac`. Both are required on `inventory`, `mac stop`, `mac export` and `cutover` and have no default. Source paths under `--home-root` (and `--home-root/.hermes`) are rewritten to `/opt/data`.

Build the macOS executable with `GOOS=darwin GOARCH=arm64 go build -o nemo-move-darwin .`. The hidden `mac-side export` protocol sends newline-delimited manifest records, a `null` terminator, then the tar stream. The Mac hashes the tree before streaming and checks it again during the second pass. The host emits no archive until verification and the SSH command both succeed.

`gate` exits successfully only when the phase file says `owner`. With `--wait`, it polls every 10 seconds until the phase changes to `owner` or its context is canceled. `probe` checks the phase and gateway status for the requested Kubernetes probe.

`seal` sets the ext4 append-only filesystem flag on each existing JSONL journal in the given directory. It does not create missing journals. It returns an error on macOS; run it in the Linux maintenance environment.

`inventory` prints a JSON report with Copy, Rebuild and Retire actions, including one `Copy (unknown)` row for each unknown `~/nemo/*` child. Row errors include missing or unreadable paths and credentials needing staging. The report is printed even if Copy rows fail; the command then exits nonzero. The total copy must remain under 30 GiB.

`mac stop` resolves `ssh` from `PATH` to an absolute executable path. Use `--ssh /absolute/path/to/ssh` to select a specific SSH binary. It requires an active GUI launchd domain for the SSH user. It stops and disables every launchd job named by a repeatable, required `--launchd-label <label>:<gui|system>` flag (for example `--launchd-label com.example.agent:gui`), then verifies that no configured job or known Nemo process remains. Before stopping, it checks whether any Nemo launchd label is loaded. It writes `~/.local/state/nemo-move/last-quiet` when that marker is absent or a label is loaded; an inactive rerun leaves it unchanged. GUI jobs are stopped without `sudo`; system daemons use noninteractive `sudo -n`. If that fails, the command prints the exact interactive shutdown commands and exits nonzero. Rerun it after those commands complete.

`mac export` accepts only a `user@host` target and requires the user to match `--ssh-user`. It first attempts credential staging with `sudo -n` on every run. If that fails, it uses existing staging only if it is newer than `last-quiet`; otherwise it prints one root staging command and exits. A missing `last-quiet` also refuses export. The host verifies the archive in a spool before emitting any output; `--spool-dir` selects its directory and defaults to the system temporary directory. Ensure that directory has enough free space. The output tar starts with `.nemo-move-manifest.json`.

`verify` reads the manifest, recomputes each copied path's type, mode, size, digest, and symlink target, checks owners, and probes read and execute access in one child process running as the requested user. `--as` accepts a user name or a numeric UID; a numeric UID also selects the same numeric GID. The child validates both Google OAuth credential trees and runs `jj status` in each repository under `opt/data/nemo/repos/`. The roots in the manifest are relative to `--root`, for example `opt/data` or `control/nemo-page`.

`cutover` holds nemo at zero, streams the verified export through the maintenance pod, rewrites source home paths, and verifies the resulting manifest as the runtime user before committing ownership. A rerun reuses a live maintenance Pod and NetworkPolicy only when they match the wanted spec, and replaces a drifted one. `close` restores the replica count that the first `open` saw, and removes the annotation that records it. `--resume` is accepted only for a record in the committing state. Before extraction, it confirms the record is started and the phase is blocked, then removes the manifest's top-level data destinations and leftover incoming journals so a partial copy can be retried from the stopped source. Paging journals use their separate replacement path. A failed extract or verification leaves ownership blocked. `restore` accepts DNS-1123 backup and node names, resumes matching backup-labelled claims using their saved recovery annotations, and rejects a different backup identity. It polls each Longhorn volume for up to two hours and fails immediately on faulte…

Only cron files, `config.yaml`, `.env`, scripts and symlink targets have source home paths rewritten. Other files, including repository stores, are copied byte for byte and verified by digest. Extraction reserves `/opt/data/.nemo-move-tmp` for scratch and clears stale scratch at the start. The scan excludes this directory and the root-owned `/opt/data/lost+found`. The operator archive spool uses a private per-run directory removed on exit.
