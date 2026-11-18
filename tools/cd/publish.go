// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var (
	errGuard        = errors.New("release commit must change only " + manifestFile)
	errPushRejected = errors.New("push rejected on every attempt")
	errStaleRelease = errors.New("a newer Release already covers this source")
)

// guard refuses a commit unless it has one parent and changes only release-manifest.json.
func guard(ctx context.Context, repo, commit string) error {
	parents, err := git(ctx, repo, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return err
	}
	if n := len(strings.Fields(parents)) - 1; n != 1 {
		return fmt.Errorf("%w: %s has %d parents", errGuard, commit, n)
	}
	changed, err := git(ctx, repo, "diff-tree", "--no-commit-id", "--name-only", "-r", commit+"^", commit)
	if err != nil {
		return err
	}
	files := lines(changed)
	if len(files) != 1 || files[0] != manifestFile {
		return fmt.Errorf("%w: %s changes %q", errGuard, commit, files)
	}
	return nil
}

type publishOptions struct {
	repo     string
	remote   string
	branch   string
	attempts int
	manifest *manifestInput
	// beforePush lets tests move the remote branch between commit and push.
	beforePush func(attempt int) error
	// commit builds the release commit; nil means commitManifest. Tests replace it.
	commit func(ctx context.Context, repo, parent string, data []byte, tag string) (string, error)
}

// publish commits the manifest on the fetched branch tip and pushes it, retrying
// on a fresh tip when the push is rejected because the branch moved.
func publish(ctx context.Context, o publishOptions) (string, error) {
	data, err := o.manifest.build()
	if err != nil {
		return "", err
	}
	var source string
	{
		var m Manifest
		if err := m.UnmarshalJSON(data); err != nil {
			return "", err
		}
		source = m.Source
	}
	ref := "refs/remotes/" + o.remote + "/" + o.branch
	for attempt := 1; attempt <= o.attempts; attempt++ {
		if _, err := git(ctx, o.repo, "fetch", "--no-tags", o.remote,
			"+refs/heads/"+o.branch+":"+ref, "+refs/tags/nebula-*:refs/tags/nebula-*"); err != nil {
			return "", err
		}
		if err := checkNewestRelease(ctx, o.repo, source); err != nil {
			return "", err
		}
		tip, err := git(ctx, o.repo, "rev-parse", ref)
		if err != nil {
			return "", err
		}
		ok, err := isAncestor(ctx, o.repo, source, tip)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("%w: source %s is not on %s tip %s", errNotDescendant, source, o.branch, tip)
		}
		// Regenerate on each tip: rebuild the manifest bytes after any retry so a
		// changed plan or asset set fails here, not after the push.
		if attempt > 1 {
			if data, err = o.manifest.build(); err != nil {
				return "", err
			}
		}
		makeCommit := o.commit
		if makeCommit == nil {
			makeCommit = commitManifest
		}
		commit, err := makeCommit(ctx, o.repo, tip, data, o.manifest.tag)
		if err != nil {
			return "", err
		}
		if err := guard(ctx, o.repo, commit); err != nil {
			return "", err
		}
		if o.beforePush != nil {
			if err := o.beforePush(attempt); err != nil {
				return "", err
			}
		}
		retry, err := push(ctx, o.repo, o.remote, commit+":refs/heads/"+o.branch)
		if err == nil {
			return commit, nil
		}
		if !retry {
			return "", err
		}
	}
	return "", fmt.Errorf("%w (%d attempts)", errPushRejected, o.attempts)
}

// checkNewestRelease refuses unless the newest nebula-* tag's source is an
// ancestor of source, so a re-run of an older run never rolls the manifest back.
func checkNewestRelease(ctx context.Context, repo, source string) error {
	tag, prev, _, err := newestRelease(ctx, repo)
	if err != nil || tag == "" {
		return err
	}
	ok, err := isAncestor(ctx, repo, prev.Source, source)
	if err != nil {
		return err
	}
	if !ok || prev.Source == source {
		return fmt.Errorf("%w: %s has source %s; this run tested %s", errStaleRelease, tag, prev.Source, source)
	}
	return nil
}

// push pushes refspec. It reports retry=true only for a non-fast-forward
// rejection (main moved); a ruleset or hook denial fails at once.
func push(ctx context.Context, repo, remote, refspec string) (bool, error) {
	command := exec.CommandContext(ctx, "git", "push", "--porcelain", remote, refspec) //nolint:gosec // Fixed binary, no shell.
	command.Dir = repo
	out, err := command.CombinedOutput()
	if err == nil {
		return false, nil
	}
	text := string(out)
	retry := strings.Contains(text, "[rejected]") &&
		(strings.Contains(text, "non-fast-forward") || strings.Contains(text, "fetch first"))
	return retry, fmt.Errorf("git push %s %s: %w: %s", remote, refspec, err, strings.TrimSpace(text))
}

// commitManifest writes data as release-manifest.json on parent's tree through a
// private index, so the working tree and HEAD are never touched.
func commitManifest(ctx context.Context, repo, parent string, data []byte, tag string) (string, error) {
	dir, err := os.MkdirTemp("", "cd-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	blob, err := gitRun(ctx, repo, nil, data, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	if _, err := gitRun(ctx, repo, env, nil, "read-tree", parent); err != nil {
		return "", err
	}
	if _, err := gitRun(ctx, repo, env, nil, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+manifestFile); err != nil {
		return "", err
	}
	tree, err := gitRun(ctx, repo, env, nil, "write-tree")
	if err != nil {
		return "", err
	}
	message := "release: " + tag + " [skip ci]"
	return git(ctx, repo, "commit-tree", tree, "-p", parent, "-m", message)
}
