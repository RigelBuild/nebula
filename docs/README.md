# Design records

This directory holds Rigel's public design records. Everything under `docs/` is public.

## Layout

- `docs/designs/<project>/` holds one public project's records. A fork's
  directory is named after its repository with no owner prefix: the records
  for `RigelBuild/oh-my-pi` live in `docs/designs/oh-my-pi/`. Fork records go
  here, not in the fork repository.
- `docs/designs/<project>/decisions/<area>/DL-NNN.md` holds one decision per
  file.

`tools/design-ledger-gate` discovers project directories from
`design-ledger.config.json`, so a new project needs no config edit. CI runs the
gate on pull requests.

## Public-only rule

Do not name, cite, quote, or link a private repository. Describe a capability by
what it does, not by where a private product runs it.

`tools/ref-gate` scans every tracked file with `tools/ref-gate/ref-gate.config.json`
and fails CI on a hit. A green scan does not prove a record is clean. Read your
record for private references the scan cannot see, such as a private path with
no repository name in it.

## Claiming decision IDs

Claim an ID before you create its decision file. The counter uses one surface,
`designs`, for every project.

```sh
CGO_ENABLED=0 DL_CLAIM_TOKEN_FILE=/path/to/token \
  go -C tools/dl-claim run . --config "$PWD/design-ledger.claim.config.json" \
  --ref RIG-<n> --lane <bookmark> --surface designs
```

The claim reads the token from the file named in `DL_CLAIM_TOKEN_FILE` when that
file exists and is non-empty. Otherwise it reads `DL_CLAIM_TOKEN`. Pass
`--config` as an absolute path, because `go -C` changes the working directory.
Use the returned ID in `docs/designs/<project>/decisions/<area>/DL-NNN.md`.

Do not rerun a failed claim blindly. The counter may already have consumed an ID.
