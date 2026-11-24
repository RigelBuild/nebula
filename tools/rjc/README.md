# rjc

Opens, updates, and lands GitHub pull requests from a jj repository. It creates issue-named bookmarks, checks PR titles and bodies, runs or records pre-push hooks, and pushes stacks through `jj-vine`.

## Profiles are required

rjc has no built-in repository list. Each run reads a profile file that the caller supplies:

```bash
rjc --profiles <file.cue> submit --title "feat(scope): subject"
RJC_PROFILES=<file.cue> rjc status
```

`--profiles` wins over `RJC_PROFILES`. When neither is set, rjc exits 2 with `no profile file: pass --profiles <file> or set RJC_PROFILES`.

rjc uses the `repos` entry matching the `origin` remote, or the optional `default` profile when no repository entry matches. The repository owner must appear in `owners`. Without a default, an unlisted repository is refused before any write: `repository <owner>/<name> is not listed in the profile file`. The selected profile provides its own issue prefix and spec directories. The global `coAuthor` trailer can be overridden per owner with `coAuthorByOwner`. The optional `default` uses the same required `issuePrefix` and `specDirs` as a repository entry.

## Schema

The profile is CUE. rjc unifies it with the embedded schema in `schema.cue` (`#Config`) and refuses a file that does not validate.

Top-level fields:

| Field | Meaning |
| --- | --- |
| `owners` | GitHub owners rjc accepts repositories from (at least one). |
| `coAuthor` | Required global fallback `Co-authored-by` trailer for commits and PR bodies. |
| `coAuthorByOwner` | Optional owner-to-trailer map that overrides `coAuthor`. |
| `default` | Optional repository profile used when no `repos` entry matches. |
| `reviewer` | Login requested as reviewer on tracking-fork PRs. |
| `agentEmail` | jj `user.email` required in agent workspaces. |
| `mergeBotEmail` | jj `user.email` required in the merge-bot workspace. |
| `mergeBotRepoDir` | Merge-bot clone root, relative to `$HOME`. |
| `aliases` | Map from a repository name to the name rjc uses for it; may be `{}`. |
| `gateRunnerDir` | Gate-runner tool directory, relative to the repository root. |
| `rootChecksDir` | Root-checks tool directory, relative to the repository root. |
| `devenvNixpkgs` | nixpkgs reference used to start a devenv shell. |
| `repos` | Map from `owner/name` to a repository entry. |

Repository entry fields:

| Field | Default | Meaning |
| --- | --- | --- |
| `issuePrefix` | required | Issue key prefix, such as `EX` for `EX-123`. The only source of the prefix for bookmarks, titles, `--issue`, and `p0:` skip reasons. |
| `specDirs` | required | Directories holding living specs, each ending in `/`. |
| `ciGates` | `[]` | PR gates checked before push: `branch-name`, `pr-title-issue-ref`, `spec-impact`, `metadata-bounds`. |
| `pr` | `true` | Whether the repository takes pull requests. |
| `posture` | `"none"` | `none`, `owned`, or `tracking` (a fork that follows an upstream). |
| `trunk` | `"main"` | Trunk branch. A tracking fork uses `rigel-release`. |
| `mirror` | — | Upstream branch mirrored by a tracking fork; required when `posture` is `tracking`. |
| `hooks` | `true` | Whether pre-push hooks run. |
| `noHooksReason` | — | Recorded skip reason; required when `hooks` is `false`. |
| `remoteHooks` | `false` | Run hooks on a remote host. |
| `localFastTier` | `false` | Allow the local fast hook tier. |
| `hookReports` | `false` | Publish hook reports. |
| `designLabel` | `false` | Apply the design label to design PRs. |
| `baseFilesets` | `[]` | Filesets whose change forces a rebase onto trunk. |
| `skipChecks` | `[]` | Root checks a `p0:` skip still runs on the head tree. |

A minimal example is in `testdata/profile.cue`.

## Skip reasons

`--skip-checks` takes a one-line reason up to 200 characters, `main-red:<pr-or-issue>`, or `p0:<PREFIX>-<n>`, where `<PREFIX>` is the repository's `issuePrefix`. A `p0:` reason with another prefix is refused before any rebase, bookmark change, push, or `gh` call.

## Tests

`go test ./...` runs with `CGO_ENABLED=0`. Tests that drive the real `jj-vine` skip when it is not on `PATH`.
