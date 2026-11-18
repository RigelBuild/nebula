// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

var errNoSplit = errors.New("git subtree split found no history for the prefix")

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

// needsPush reports whether a mirror whose branch is at tip must take split.
// An empty tip is a mirror branch with no commits yet.
func needsPush(split, tip string) bool {
	return split != tip
}

// syncMirror splits o.prefix at HEAD and pushes it to the mirror branch only when
// the split differs from the branch tip. It reports the split and whether it pushed.
// The push is a fast-forward: a split that does not extend the tip is rejected.
func syncMirror(ctx context.Context, o mirrorOptions) (string, bool, error) {
	split, err := subtreeSplit(ctx, o.repo, o.prefix, "HEAD")
	if err != nil {
		return "", false, err
	}
	env := authEnv(o.token)
	tip, err := mirrorTip(ctx, o.repo, env, o.url, o.branch)
	if err != nil {
		return "", false, err
	}
	if !needsPush(split, tip) {
		return split, false, nil
	}
	if _, err := gitRun(ctx, o.repo, env, nil, "push", o.url, split+":refs/heads/"+o.branch); err != nil {
		return "", false, err
	}
	return split, true, nil
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
