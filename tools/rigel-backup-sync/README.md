# rigel-backup-sync

Keeps a local bare repo as a fast-forward-only mirror of the upstream `main` branch on GitHub. A user timer runs it every minute, so the mirror trails by at most one interval.

The tool fetches upstream `main` over SSH, then updates the local bare repo only when the fetched commit is a descendant of local `main`. Divergence makes the run fail and leaves local `main` unchanged. A local mirror with no commits is initialized from upstream; an up-to-date mirror is a clean no-op.

## Configuration

Every value is required. The tool has no defaults.

| Variable | Value |
| --- | --- |
| `RIGEL_BACKUP_KEY_FILE` | Path to a read-only SSH deploy key for the upstream repository. |
| `RIGEL_BACKUP_KNOWN_HOSTS` | Path to a known-hosts file that pins the upstream host key. |
| `RIGEL_BACKUP_MIRROR` | Path to the local bare mirror repository. |
| `RIGEL_BACKUP_UPSTREAM` | Git URL of the upstream repository, for example `git@example.com:example/upstream.git`. |

The deploy key has read-only access to the upstream repository. The mirror sync never pushes to GitHub.

## Rotation

Replace the deploy key on the upstream repository, write the new private key to the file that `RIGEL_BACKUP_KEY_FILE` names, then run the tool once to confirm.

## Usage

Invoke it without arguments to sync. `rigel-backup-sync --help` shows configuration without starting a sync.
