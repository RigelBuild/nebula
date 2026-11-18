# Nebula

Nebula is the public monorepo for Rigel forks, tools, and design records.
Forks live under `forks/`, first-party tools under `tools/`, and public
records under `docs/`.

## Development

Enter the Nix development shell with `nix develop`. It provides Go, Bun, Moon,
rumdl, and Biome. Moon registers each tool project from its own `moon.yml`.
Fork project manifests live inside each fork tree at `forks/<name>/moon.yml`
and are registered by the `forks/*/moon.yml` glob in `.moon/workspace.yml`.

## CI

GitHub Actions runs advisory, affected-project CI on Linux x86_64, Linux ARM64,
and Apple Silicon macOS. Same-repository pull requests use the `ci` environment.
Fork pull requests run through `pull_request_target` and the reviewer-gated
`external` environment before checking out the approved head commit.

CI reads four repository variables: `NIX_CACHE_SUBSTITUTER`,
`NIX_CACHE_PUBLIC_KEY`, `MOON_REMOTE_CACHE_HOST`, and
`MOON_REMOTE_CACHE_INSTANCE_NAME`. An unset variable leaves its setting empty.

The `ref-gate` job scans every tracked file with `tools/ref-gate` and fails on
a hit. Its allowlist is `tools/ref-gate/ref-gate.config.json`.

## Layout

- `.github/workflows/ci.yaml` — advisory affected-project CI.
- `forks/<name>/` — imported upstream source; style gates exclude this tree.
- `forks/<name>/moon.yml`, `forks/<name>/upstream.json`, and
  `forks/<name>/component.cue` — nebula-owned project manifest, sync record,
  and release component inside each fork tree; `fork-sync export` leaves them
  out of upstream patches.
- `tools/<name>/` — first-party public tools, each with its own `moon.yml`.
- `docs/designs/<project>/` — public design records.
