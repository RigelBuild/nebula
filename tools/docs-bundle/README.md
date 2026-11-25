# docs-bundle

`docs-bundle` is a CLI-only Go tool for checking publish rules and links before a site build. Its Go packages do not provide a supported library API. It does not fetch source repositories or invoke a site generator.

## Build and install

Build the binary from this directory:

```sh
go build -o docs-bundle .
```

The CLI has no runtime dependency on Bun or Node.js.

## Check a repository

```sh
docs-bundle check [--root .] [--config required|default]
```

The default config mode is `required`. It expects `docs-bundle.toml` at the repository root. Use `--config default` when a source has no publish config; all gatherable Markdown files use `/<owner>/<repo>/<path>` routes. Collection skips files in dot- or underscore-prefixed path segments, except `.github/PULL_REQUEST_TEMPLATE.md`.

The TOML file requires an `exclude` array. `[[sections]]` entries assign page prefixes or explicit files to site routes. An optional `[packages]` table configures package README routing and the allowed package pages.

`packages.readme_depth` is the maximum number of path segments for a README.md or AGENTS.md in the catch-all. `packages.allow` lists exact paths or directory prefixes ending in `/`. `packages.nested` lists top-level directories whose first two path segments form one package ID, joined with a hyphen.

The check runs on Markdown files and does not need a site generator. Exit codes:

| Code | Meaning |
| ---- | ------- |
| 0 | No errors. |
| 1 | Page collection failed (route collisions, stray or unassigned pages, an unreadable root), or broken relative links. |
| 2 | Invalid arguments, a missing or invalid `docs-bundle.toml`, or a page that cannot be read while checking links. |

