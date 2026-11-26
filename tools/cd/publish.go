// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var (
	errGuard          = errors.New("release commit must change only " + manifestFile)
	errPushRejected   = errors.New("push rejected on every attempt")
	errStaleRelease   = errors.New("a newer Release already covers this source")
	errResumeMismatch = errors.New("tag already exists with a different manifest")
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
	// stderr receives recovery diagnostics. Nil uses os.Stderr.
	stderr io.Writer
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
		if attempt > 1 {
			if data, err = o.manifest.build(); err != nil {
				return "", err
			}
		}
		existing, err := checkNewestRelease(ctx, o.repo, source, o.manifest.tag, data)
		if err != nil {
			return "", err
		}
		if existing != "" {
			return existing, nil
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
		if existing, err := findManifestCommit(ctx, o.repo, source, tip, data); err != nil {
			return "", err
		} else if existing != "" {
			return o.resumed(existing)
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

// resumed reports on stderr that publish reuses commit instead of pushing one.
func (o publishOptions) resumed(commit string) (string, error) {
	stderr := o.stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	if _, err := fmt.Fprintf(stderr, "resuming from existing manifest commit %s\n", commit); err != nil {
		return "", err
	}
	return commit, nil
}

// equalJSON reports whether two JSON values have the same canonical encoding.
func equalJSON(expected, existing []byte) (bool, error) {
	var expectedValue, existingValue any
	if err := json.Unmarshal(expected, &expectedValue); err != nil {
		return false, err
	}
	if err := json.Unmarshal(existing, &existingValue); err != nil {
		return false, err
	}
	expectedJSON, err := json.Marshal(expectedValue)
	if err != nil {
		return false, err
	}
	existingJSON, err := json.Marshal(existingValue)
	if err != nil {
		return false, err
	}
	return bytes.Equal(expectedJSON, existingJSON), nil
}

// findManifestCommit returns a guarded release-manifest commit on tip's history
// when its contents exactly match this release.
func findManifestCommit(ctx context.Context, repo, source, tip string, data []byte) (string, error) {
	commit, err := git(ctx, repo, "log", "-1", "--format=%H", tip, "--", manifestFile)
	if err != nil || commit == "" {
		return "", err
	}
	manifest, err := git(ctx, repo, "show", commit+":"+manifestFile)
	if err != nil {
		return "", err
	}
	matches, err := equalJSON(data, []byte(manifest))
	if err != nil {
		return "", err
	}
	if !matches {
		return "", nil
	}
	if err := guard(ctx, repo, commit); err != nil {
		return "", err
	}
	parent, err := git(ctx, repo, "rev-parse", commit+"^")
	if err != nil {
		return "", err
	}
	ok, err := isAncestor(ctx, repo, source, parent)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	return commit, nil
}

// checkNewestRelease returns the existing tag commit for this exact release and
// refuses stale runs so an older run never rolls the manifest back.
func checkNewestRelease(ctx context.Context, repo, source, plannedTag string, data []byte) (string, error) {
	tag, prev, previous, err := newestRelease(ctx, repo)
	if err != nil || tag == "" {
		return "", err
	}
	if tag == plannedTag {
		if prev.Source != source {
			return "", fmt.Errorf("%w: %s has source %s; this run tested %s", errStaleRelease, tag, prev.Source, source)
		}
		matches, err := equalJSON(data, []byte(previous))
		if err != nil {
			return "", err
		}
		if !matches {
			return "", fmt.Errorf("%w: tag %s already exists with a different manifest; re-run all jobs on a new commit", errResumeMismatch, tag)
		}
		commit, err := git(ctx, repo, "rev-parse", tag+"^{commit}")
		if err != nil {
			return "", err
		}
		return commit, nil
	}
	ok, err := isAncestor(ctx, repo, prev.Source, source)
	if err != nil {
		return "", err
	}
	if !ok || prev.Source == source {
		return "", fmt.Errorf("%w: %s has source %s; this run tested %s", errStaleRelease, tag, prev.Source, source)
	}
	return "", nil
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
