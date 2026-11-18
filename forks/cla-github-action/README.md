# Sealed Security CLA GitHub Action

> [!NOTE]
> **Maintained by [Sealed Security](https://sealedsecurity.com) for use on [`sealedsecurity/seal`](https://github.com/sealedsecurity/seal).**
>
> This is a fork of [`contributor-assistant/github-action`](https://github.com/contributor-assistant/github-action), which the upstream maintainer [archived in March 2026](https://github.com/contributor-assistant/github-action/commit/58daaf8). All the original code, mechanics, and contributor attribution are preserved; we maintain it here so we can pin a stable SHA and fix bugs that affect us without depending on a service we don't control.
>
> External users are welcome to depend on this fork, but be aware that the only support guarantee is "whatever Sealed Security needs." If you need a more general-purpose CLA action, fork ours (or fork the original) and maintain it for your own use.

---

# Handling CLAs and DCOs via GitHub Action

A GitHub Action that handles the legal side of contributions to a repository: contributors sign the CLA (or DCO) by commenting on the pull request, and their signature is stored in a JSON file in this repository (or a remote one). No centrally managed database; no external service dependency once the action is pinned to a SHA.

### Features
1. Decentralized data storage (signatures live in your own repo).
1. Fully integrated within the GitHub environment.
1. No user interface required — sign by commenting on the PR.
1. Signatures are stored in a file inside the repository or in a remote repository.
1. Remote repository can be private.
1. Versioning of signatures.

## Configure the CLA action

#### 1. Add the workflow file at `.github/workflows/cla.yml`

```yml
name: "CLA Assistant"
on:
  issue_comment:
    types: [created]
  pull_request_target:
    types: [opened, closed, synchronize]

# Explicit permissions in case your GITHUB_TOKEN workflow permissions
# are read-only at the repository level.
permissions:
  actions: write
  contents: write   # 'read' if signatures are stored in a remote repository
  pull-requests: write
  statuses: write

jobs:
  CLAAssistant:
    runs-on: ubuntu-latest
    steps:
      - name: "CLA Assistant"
        if: (github.event.comment.body == 'recheck' || github.event.comment.body == 'I have read the CLA Document and I hereby sign the CLA') || github.event_name == 'pull_request_target'
        # Pin to a specific commit on this fork's main branch.
        uses: sealedsecurity/cla-github-action@<PIN_A_COMMIT_SHA>
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          # Only required when storing signatures in a remote repository/organization.
          # PERSONAL_ACCESS_TOKEN: ${{ secrets.PERSONAL_ACCESS_TOKEN }}
        with:
          path-to-signatures: 'signatures/version1/cla.json'
          path-to-document: 'https://github.com/sealedsecurity/seal/blob/main/CLA.md'
          # The signatures branch should not be protected.
          branch: 'main'
          allowlist: user1,bot*
```

#### 2. Pull Request event triggers the CLA workflow

The action runs on `pull_request_target` (so it can write back to the PR even from a fork) for `opened`, `synchronize`, and `closed`. On `closed`, it locks the PR conversation so contributors can't edit or delete their signature comment after merge. Locking is optional (`lock-pullrequest-aftermerge: false`).

#### 3. Signing the CLA

When the workflow finds a contributor who hasn't signed, it posts a comment asking them to sign and fails the PR status check. The contributor signs by commenting **"I have read the CLA Document and I hereby sign the CLA"** on the PR. The status check then flips to success.

#### 4. Signatures stored in a JSON file

After signing, the contributor's signature and metadata are stored in the JSON file configured by `path-to-signatures` (default `signatures/version1/cla.json`). The action creates the file on first sign; do **not** create it manually.

To store signatures in a remote repository instead, set `remote-organization-name` and `remote-repository-name` and provide a `PERSONAL_ACCESS_TOKEN` secret with `repo` scope.

#### 5. Users and bots in the allowlist

The `allowlist` input takes a comma-separated list of usernames who are exempt from signing. Useful for organization members and bots that can't sign (e.g. `dependabot[bot]`). Wildcards work — `bot*` exempts every bot.

#### 6. Adding a Personal Access Token

Only required when using a remote signatures repository. Create a [Personal Access Token](https://docs.github.com/en/github/authenticating-to-github/creating-a-personal-access-token) with `repo` scope and store it as a repository secret named `PERSONAL_ACCESS_TOKEN`.

### Environment variables

| Name                   | Requirement | Description |
| ---------------------- | ----------- | ----------- |
| `GITHUB_TOKEN`         | required    | The built-in `${{ secrets.GITHUB_TOKEN }}`. Used for GitHub API calls (posting comments, setting statuses, committing the signatures file). |
| `PERSONAL_ACCESS_TOKEN`| optional    | Required only when storing signatures in a remote repository/organization. PAT with `repo` scope, stored as a repository secret. |

### Inputs

| Name                            | Requirement | Description | Example |
| ------------------------------- | ----------- | ----------- | ------- |
| `path-to-document`              | required    | Full URL to the document to sign. Can be a file in the repository or a Gist. | `https://github.com/sealedsecurity/seal/blob/main/CLA.md` |
| `path-to-signatures`            | optional    | Path to the JSON signatures file in the (local or remote) repository. | `signatures/version1/cla.json` |
| `branch`                        | optional    | Branch where signatures are stored. Default `master`. | `main` |
| `allowlist`                     | optional    | Comma-separated users/bots that don't have to sign. Wildcards supported. | `user1,user2,bot*` |
| `remote-repository-name`        | optional    | Name of the remote signatures repository. | |
| `remote-organization-name`      | optional    | Name of the remote signatures organization. | |
| `create-file-commit-message`    | optional    | Commit message when the signatures file is first created. | `Creating file for storing CLA Signatures.` |
| `signed-commit-message`         | optional    | Commit message when a new contributor signs. | `$contributorName has signed the CLA in $pullRequestNo` |
| `custom-notsigned-prcomment`    | optional    | Intro text in the PR comment asking new contributors to sign. | |
| `custom-pr-sign-comment`        | optional    | The exact phrase a contributor comments to sign. | `I have read the Developer Terms Document and I hereby accept the Terms` |
| `custom-allsigned-prcomment`    | optional    | PR comment when everyone in the PR has signed. | |
| `lock-pullrequest-aftermerge`   | optional    | Lock the PR conversation after merge to preserve signature evidence. Default `true`. | `false` |
| `suggest-recheck`               | optional    | Suggest `recheck` in the action's PR comment. Default `true`. | `false` |
| `use-dco-flag`                  | optional    | Set to `true` to use DCO instead of CLA. Default `false`. | `true` |

## Upstream attribution

This action is a fork of [`contributor-assistant/github-action`](https://github.com/contributor-assistant/github-action), originally created by SAP and the CLA Assistant project. See the upstream contributor list for everyone who built and maintained this code before the upstream archival. We maintain the fork to preserve a working CLA flow for the [`sealedsecurity/seal`](https://github.com/sealedsecurity/seal) open-source repository now that the upstream is read-only.

## License

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the License. You may obtain a copy of the License at:

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific language governing permissions and limitations under the License.
