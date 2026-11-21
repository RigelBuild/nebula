# transcript-archive

Inventories agent transcript files and archives them to a restic repository.

## Commands

```sh
transcript-archive inventory --host <name> --config <path> --out <file> [--previous <file>] [--delta <file>]
transcript-archive pass      --host <name> --config <path> --state-dir <dir> --repo <repo> --escrow-out <file> --escrow-recipient <age1…> [--restic <path>]
transcript-archive coverage  --state-dir <dir> --scope <host> --repo <repo> [--restic <path>]
transcript-archive get       --repo <repo> --escrow <file> --identity <file> --snapshot <id> --record <stem> --out <dir>
transcript-archive fence     [--agent-dir <dir>]... [--wait <duration>]
transcript-archive rawcap    --repo <repo> --state-dir <dir> --cap-bytes <n>
```

## Required inputs

Every deployment value is a flag with no default. The tool refuses to run
without it, and a missing required flag exits non-zero.

| Input | Flag | Used by | Rule |
| --- | --- | --- | --- |
| Host config | `--config` | `inventory`, `pass` | Required. A CUE file that declares `hosts`. The schema is compiled into the binary, so no schema file sits beside it. |
| Host name | `--host` | `inventory`, `pass` | Required. A key under `hosts` in the config. |
| Escrow recipient | `--escrow-recipient` | `pass` | Required. An age X25519 public key. Nothing is baked into the binary. |
| Repository | `--repo` | `pass`, `coverage`, `get`, `rawcap` | Required. A local path or any restic backend URL. |

Bucket names, endpoints and secret paths are inputs too. The binary has no
default for any of them.

## Config

The host config is CUE. Each host sets `state` (`measured`, `unmeasured` or
`empty`) and lists `sources`, `watch`, `exclude` and `finalOnly`. The config in
`testdata/archive.cue` is a synthetic example (`example-host`) for tests. Use
your own config for a real host; keep it out of this repository.

## Host timer: `pass`

A host's user timer runs one pass per tick. A systemd user unit does not expand `~`, so use `%h` or absolute paths:

```ini
ExecStart=/path/to/transcript-archive pass \
  --host <host> \
  --config /path/to/archive.cue \
  --state-dir %h/.local/state/transcript-archive/<host> \
  --repo <restic-repo-url> \
  --escrow-out /path/to/escrow/<host>.age \
  --escrow-recipient <age1-public-key>
```

Optional: `--restic /path/to/restic`. The repository may be a local path or any restic backend (`s3:`, `rest:`, `b2:`, `azure:`, `gs:`, `sftp:`, `swift:`, `rclone:`).

Each run takes these steps:

1. Take the state directory's lock, so a second run fails instead of interleaving.
2. Open the scope state. The first run generates the repository password.
3. Walk the host's configured sources under `HOME` against the last committed scan.
4. Recover any pass that started without an outcome.
5. Record the scan and run one `restic backup`. On success the journal gets one `path-acked` line per pending path.

- **Credentials.** For an S3 repository, export `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`. They pass through to restic. All `RESTIC_*` variables are ignored; the password comes only from `<state-dir>/password`.
- **Escrow.** Before the repository is created, the password is age-encrypted to `--escrow-recipient` and written to `--escrow-out`, create-exclusive and fsynced. `<escrow-out>.sha256` records the plaintext digest. A later run reuses the file only if the digest matches. A different password is refused, and the file is never overwritten.
- **Exit status.** 0 means the pass was acked. Any other result exits non-zero with one line on stderr. The next run recovers it.
- **`--restic`** selects the restic binary; the default is `restic` on `PATH`. `coverage` takes the same flag.

## Before deleting: `coverage`

Run `coverage` against the repository before you delete any local transcript. It needs only the state directory:

```sh
transcript-archive coverage --state-dir <dir> --scope <host> --repo <repo> [--restic <path>]
```

It reads `scan-current.jsonl`, `journal.jsonl` and `password`. It certifies the last scan's content, not the live file: a file changed after the last pass can still pass, so a deleter must re-check each file's size and mtime against its scan line first.

`pass` refuses to start if the state or scratch directory resolves (following symlinks) under a source root.

## Development

```sh
go test ./...
```

Tests build their own throwaway age keys and fixtures. None of them need a
real host, repository or escrow key.
