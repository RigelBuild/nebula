package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type exportOptions struct {
	Name   string
	Range  string
	Topic  string
	Outbox string
}

var topicName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]*$`)

func exportFork(ctx context.Context, root string, options exportOptions, out io.Writer) (resultErr error) {
	if err := validateName(options.Name); err != nil {
		return err
	}
	if options.Range == "" || options.Topic == "" {
		return errors.New("--range and --topic are required")
	}
	if !topicName.MatchString(options.Topic) || strings.Contains(options.Topic, "..") || strings.Contains(options.Topic, "//") {
		return fmt.Errorf("invalid topic %q", options.Topic)
	}
	if err := validateOutbox(options.Outbox); err != nil {
		return err
	}
	parts := strings.Split(options.Range, "..")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("range must use A..B syntax, got %q", options.Range)
	}
	if err := ensureClean(ctx, root); err != nil {
		return fmt.Errorf("export requires a clean worktree: %w", err)
	}
	recordPath := filepath.Join(root, "forks", options.Name, "upstream.json")
	record, err := readUpstreamRecord(recordPath)
	if err != nil {
		return err
	}
	temporary, err := os.MkdirTemp("", "nebula-fork-export-")
	if err != nil {
		return fmt.Errorf("create temporary outbox repository: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(temporary)) }()
	if _, err := git(ctx, temporary, nil, "init", "--bare", "-q"); err != nil {
		return fmt.Errorf("initialize temporary export repository: %w", err)
	}
	if _, err := git(ctx, temporary, nil, "fetch", "--quiet", "--", record.Repo, "refs/heads/"+record.Branch); err != nil {
		return fmt.Errorf("fetch recorded upstream branch: %w", err)
	}
	if _, err := git(ctx, temporary, nil, "cat-file", "-e", record.Commit+"^{commit}"); err != nil {
		return fmt.Errorf("recorded upstream base %s is unavailable from %s:%s: %w", short(record.Commit), record.Repo, record.Branch, err)
	}
	if _, err := git(ctx, temporary, nil, "update-ref", "refs/fork-sync/base", record.Commit); err != nil {
		return fmt.Errorf("record upstream base ref: %w", err)
	}
	if options.Outbox == "" {
		options.Outbox = "https://github.com/RigelBuild/" + options.Name + ".git"
	}
	left, right := parts[0], parts[1]
	if _, err := git(ctx, root, nil, "rev-parse", "--verify", left+"^{commit}"); err != nil {
		return fmt.Errorf("resolve range start: %w", err)
	}
	if _, err := git(ctx, root, nil, "rev-parse", "--verify", right+"^{commit}"); err != nil {
		return fmt.Errorf("resolve range end: %w", err)
	}
	patch, err := git(ctx, root, nil, "diff", "--binary", "--find-renames", "--full-index", left, right, "--", "forks/"+options.Name, ":(exclude)forks/"+options.Name+"/moon.yml", ":(exclude)forks/"+options.Name+"/upstream.json", ":(exclude)forks/"+options.Name+"/component.cue")
	if err != nil {
		return fmt.Errorf("create nebula export diff: %w", err)
	}
	if len(patch) == 0 {
		return errors.New("requested range contains no changes under the fork tree")
	}
	worktree := filepath.Join(temporary, "worktree")
	if err := os.Mkdir(worktree, 0o755); err != nil {
		return fmt.Errorf("create export worktree: %w", err)
	}
	if _, err := git(ctx, worktree, nil, "init", "-q"); err != nil {
		return fmt.Errorf("initialize export worktree: %w", err)
	}
	if _, err := git(ctx, worktree, nil, "fetch", "--quiet", temporary, "refs/fork-sync/base"); err != nil {
		return fmt.Errorf("fetch upstream base to worktree: %w", err)
	}
	if _, err := git(ctx, worktree, nil, "switch", "-q", "--detach", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("checkout upstream base: %w", err)
	}
	if err := transferPatchBlobs(ctx, root, worktree, patch); err != nil {
		return fmt.Errorf("prepare three-way patch blobs: %w", err)
	}
	if _, err := git(ctx, worktree, patch, "apply", "--3way", "-p3", "-"); err != nil {
		return fmt.Errorf("apply nebula changes to outbox tree: %w", err)
	}
	if _, err := git(ctx, worktree, nil, "add", "-A"); err != nil {
		return fmt.Errorf("stage exported changes: %w", err)
	}
	tree, err := git(ctx, worktree, nil, "write-tree")
	if err != nil {
		return fmt.Errorf("write exported tree: %w", err)
	}
	commit, err := git(ctx, worktree, nil, "-c", "user.name=mintaka", "-c", "user.email=mintaka@rigel.build", "commit-tree", "-m", "Export "+options.Name+" changes from nebula "+options.Range, "-p", record.Commit, strings.TrimSpace(string(tree)))
	if err != nil {
		return fmt.Errorf("create export commit: %w", err)
	}
	sha := strings.TrimSpace(string(commit))
	ref := "refs/heads/upstream/" + options.Topic
	if _, err := git(ctx, worktree, nil, "push", "--", options.Outbox, sha+":"+ref); err != nil {
		return fmt.Errorf("push export branch: %w", err)
	}
	compare, err := outboxCompareURL(options.Outbox, options.Topic, record.Commit)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "pushed %s to %s\ncompare: %s\n", short(sha), ref, compare)
	return err
}

func outboxCompareURL(remote, topic, base string) (string, error) {
	parsed, err := url.Parse(remote)
	if err != nil {
		return "", fmt.Errorf("parse outbox URL: %w", err)
	}
	path := strings.TrimSuffix(strings.TrimSuffix(parsed.Path, ".git"), "/")
	if parsed.Host == "github.com" {
		return "https://github.com" + path + "/compare/main..." + url.PathEscape("upstream/"+topic) + "?expand=1", nil
	}
	return "outbox " + remote + " branch upstream/" + topic + " (base " + short(base) + ")", nil
}
