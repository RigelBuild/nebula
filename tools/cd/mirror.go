// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

var errNoSplit = errors.New("git subtree split found no history for the prefix")

// mirrorState is the outcome of one syncMirror call. The CLI prints it.
type mirrorState string

const (
	mirrorUnchanged mirrorState = "unchanged"
	mirrorPushed    mirrorState = "pushed"
)

const mirrorCommitIdentity = "rigelbuild-nebula-release[bot]"

// mirrorOptions names one sync-out mirror: the repository path to split out,
// the mirror's clone URL and branch, and the token that may push to it.
type mirrorOptions struct {
	repo, prefix, url, branch, token string
}

// subtreeSplit returns the commit that holds prefix's history at commit, with
// prefix as the root of its tree. Commits that do not touch prefix are dropped.
func subtreeSplit(ctx context.Context, repo, prefix, commit string) (string, error) {
	split, err := git(ctx, repo, "subtree", "split", "--prefix="+prefix, commit)
	if err != nil {
		return "", err
	}
	if split == "" {
		return "", fmt.Errorf("%w: %s at %s", errNoSplit, prefix, commit)
	}
	return split, nil
}

// syncMirror splits o.prefix at HEAD and advances the mirror with a plain push.
// An equal tip is unchanged. When the mirror tip is not an ancestor of the
// split, a bridge merge preserves both histories and keeps later pushes fast-forwardable.
func syncMirror(ctx context.Context, o mirrorOptions, warn io.Writer) (string, mirrorState, error) {
	split, err := subtreeSplit(ctx, o.repo, o.prefix, "HEAD")
	if err != nil {
		return "", "", err
	}
	env := authEnv(o.token)
	tip, err := mirrorTip(ctx, o.repo, env, o.url, o.branch)
	if err != nil {
		return "", "", err
	}
	if tip == split {
		return split, mirrorUnchanged, nil
	}
	pushTarget := split
	if tip != "" {
		// Fetch the tip so its commit exists locally for the ancestry check.
		if _, err := gitRun(ctx, o.repo, env, nil, "fetch", "--no-tags", "-q", o.url, "refs/heads/"+o.branch); err != nil {
			return "", "", err
		}
		ancestor, err := isAncestor(ctx, o.repo, tip, split)
		if err != nil {
			return "", "", err
		}
		if !ancestor {
			continuation, sameSplit, err := isBridgeContinuation(ctx, o.repo, tip, split)
			if err != nil {
				return "", "", err
			}
			if continuation && sameSplit {
				return split, mirrorUnchanged, nil
			}
			if !continuation {
				if _, err := fmt.Fprintf(
					warn,
					"::warning::mirror %s branch tip %s is not an ancestor of split %s; "+
						"pushing a bridge merge to preserve both histories\n",
					o.url,
					tip,
					split,
				); err != nil {
					return "", "", err
				}
			}
			pushTarget, err = gitRun(
				ctx,
				o.repo,
				[]string{
					"GIT_AUTHOR_NAME=" + mirrorCommitIdentity,
					"GIT_AUTHOR_EMAIL=" + mirrorCommitIdentity + "@users.noreply.github.com",
					"GIT_COMMITTER_NAME=" + mirrorCommitIdentity,
					"GIT_COMMITTER_EMAIL=" + mirrorCommitIdentity + "@users.noreply.github.com",
				},
				nil,
				"commit-tree",
				split+"^{tree}",
				"-p",
				tip,
				"-p",
				split,
				"-m",
				"Merge history into the nebula subtree split",
			)
			if err != nil {
				return "", "", err
			}
		}
	}
	if _, err := gitRun(ctx, o.repo, env, nil, "push", o.url, pushTarget+":refs/heads/"+o.branch); err != nil {
		return "", "", err
	}
	return split, mirrorPushed, nil
}

// isBridgeContinuation reports whether tip's second parent is already in split's history.
// It also reports whether that parent is exactly split.
func isBridgeContinuation(ctx context.Context, repo, tip, split string) (bool, bool, error) {
	parents, err := git(ctx, repo, "rev-list", "--parents", "-n", "1", tip)
	if err != nil {
		return false, false, err
	}
	fields := strings.Fields(parents)
	if len(fields) < 3 {
		return false, false, nil
	}
	continuation, err := isAncestor(ctx, repo, fields[2], split)
	return continuation, fields[2] == split, err
}

// mirrorTip returns the SHA of url's branch, or "" when the branch does not exist.
func mirrorTip(ctx context.Context, repo string, env []string, url, branch string) (string, error) {
	out, err := gitRun(ctx, repo, env, nil, "ls-remote", url, "refs/heads/"+branch)
	if err != nil || out == "" {
		return "", err
	}
	fields := strings.Fields(lines(out)[0])
	return fields[0], nil
}

// authEnv passes token to git as an HTTP basic header for github.com, the way
// actions/checkout does, so the token never appears in a command line or a URL.
func authEnv(token string) []string {
	if token == "" {
		return nil
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + basic,
	}
}
