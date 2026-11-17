# Nebula

Nebula is the public monorepo for Rigel forks, tools, and design records.
Forks live under `forks/`, first-party tools under `tools/`, and public
records under `docs/`.

## Development

Enter the Nix development shell with `nix develop`. It provides Go, Bun, Moon,
rumdl, and Biome. Moon registers each tool and documentation project from its
own `moon.yml`; fork project manifests live beside, not inside, imported source
and are registered explicitly in `.moon/workspace.yml`.

## CI

GitHub Actions runs advisory, affected-project CI on Linux x86_64, Linux ARM64,
and Apple Silicon macOS. Same-repository pull requests use the `ci` environment.
Fork pull requests run through `pull_request_target` and the reviewer-gated
`external` environment before checking out the approved head commit.

## Layout

- `.github/workflows/ci.yaml` — advisory affected-project CI.
- `forks/<name>/` — imported upstream source; style gates exclude this tree.
- `forks/<name>.moon.yml` — explicitly registered Moon project manifests,
  outside imported source trees.
- `tools/<name>/` — first-party public tools, each with its own `moon.yml`.
- `docs/designs/<project>/` — public design records.
