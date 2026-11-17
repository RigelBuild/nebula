package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type importOptions struct {
	Name           string
	Fork           string
	Ref            string
	Upstream       string
	UpstreamBranch string
	Base           string
}

type upstreamRecord struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Commit string `json:"commit"`
}

var safeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// readUpstreamRecord loads a fork's upstream.json and checks it. Its repo is passed to git,
// so it is validated here, before any fetch.
func readUpstreamRecord(path string) (upstreamRecord, error) {
	var record upstreamRecord
	data, err := os.ReadFile(path)
	if err != nil {
		return record, fmt.Errorf("read upstream record: %w", err)
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, fmt.Errorf("decode upstream record: %w", err)
	}
	if record.Repo == "" || record.Branch == "" || record.Commit == "" {
		return record, errors.New("upstream record must contain repo, branch and commit")
	}
	return record, validateRepo(record.Repo)
}

func importFork(ctx context.Context, root string, options importOptions) (resultErr error) {
	if err := validateName(options.Name); err != nil {
		return err
	}
	if options.Fork == "" || options.Upstream == "" || options.UpstreamBranch == "" {
		return errors.New("--fork, --upstream and --upstream-branch are required")
	}
	if err := validateRepo(options.Upstream); err != nil {
		return err
	}
	if options.Ref == "" {
		options.Ref = "rigel-release"
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create nebula worktree root: %w", err)
	}
	if _, err := git(ctx, root, nil, "rev-parse", "--show-toplevel"); err != nil {
		return fmt.Errorf("import requires a git worktree: %w", err)
	}
	if err := ensureClean(ctx, root); err != nil {
		return fmt.Errorf("import requires a clean worktree: %w", err)
	}
	if _, err := os.Stat(filepath.Join(root, "forks", options.Name)); err == nil {
		return fmt.Errorf("fork %q is already imported", options.Name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check fork destination: %w", err)
	}
	temporary, err := os.MkdirTemp("", "nebula-fork-import-")
	if err != nil {
		return fmt.Errorf("create temporary clone: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(temporary)) }()
	forkURL := forkRepoURL(options.Fork)
	if err := validateRepo(forkURL); err != nil {
		return fmt.Errorf("--fork: %w", err)
	}
	if _, err := git(ctx, temporary, nil, "init", "--bare", "-q"); err != nil {
		return fmt.Errorf("initialize temporary repository: %w", err)
	}
	if _, err := git(ctx, temporary, nil, "fetch", "--quiet", "--", forkURL, options.Ref); err != nil {
		return fmt.Errorf("fetch fork ref %s: %w", options.Ref, err)
	}
	forkBytes, err := git(ctx, temporary, nil, "rev-parse", "FETCH_HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("resolve fork ref: %w", err)
	}
	forkCommit := strings.TrimSpace(string(forkBytes))
	if _, err := git(ctx, temporary, nil, "fetch", "--quiet", "--", options.Upstream, "refs/heads/"+options.UpstreamBranch); err != nil {
		return fmt.Errorf("fetch upstream branch: %w", err)
	}
	upstreamBytes, err := git(ctx, temporary, nil, "rev-parse", "FETCH_HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("resolve upstream branch: %w", err)
	}
	upstreamTip := strings.TrimSpace(string(upstreamBytes))
	base := options.Base
	if base == "" {
		baseBytes, err := git(ctx, temporary, nil, "merge-base", forkCommit, upstreamTip)
		if err != nil {
			if gitExitCode(err) == 1 {
				return errors.New("fork and upstream have no merge-base; pass --base <sha> to select the upstream base explicitly")
			}
			return fmt.Errorf("compute upstream merge-base: %w", err)
		}
		base = strings.TrimSpace(string(baseBytes))
	}
	if _, err := git(ctx, temporary, nil, "fetch", "--quiet", "--", options.Upstream, base); err != nil {
		return fmt.Errorf("fetch upstream base %q: %w", base, err)
	}
	if _, err := git(ctx, temporary, nil, "cat-file", "-e", base+"^{commit}"); err != nil {
		return fmt.Errorf("upstream base %q is not available in fetched history: %w", base, err)
	}
	if _, err := git(ctx, temporary, nil, "merge-base", "--is-ancestor", base, upstreamTip); err != nil {
		return fmt.Errorf("upstream base %q is not an ancestor of branch tip %s: %w", base, short(upstreamTip), err)
	}
	if err := os.MkdirAll(filepath.Join(root, "forks"), 0o755); err != nil {
		return fmt.Errorf("create forks directory: %w", err)
	}
	tree, err := git(ctx, temporary, nil, "archive", "--format=tar", forkCommit)
	if err != nil {
		return fmt.Errorf("archive fork tree: %w", err)
	}
	destination := filepath.Join(root, "forks", options.Name)
	if err := os.Mkdir(destination, 0o755); err != nil {
		return fmt.Errorf("create fork tree: %w", err)
	}
	extract := exec.CommandContext(ctx, "tar", "-xf", "-") //nolint:gosec // Fixed tar arguments; input is created by git archive.
	extract.Dir = destination
	extract.Stdin = bytes.NewReader(tree)
	if output, err := extract.CombinedOutput(); err != nil {
		return fmt.Errorf("extract fork tree: %w: %s", err, strings.TrimSpace(string(output)))
	}
	record := upstreamRecord{Repo: options.Upstream, Branch: options.UpstreamBranch, Commit: base}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode upstream record: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, "forks", options.Name, "upstream.json"), append(encoded, '\n'), 0o644); err != nil {
		return fmt.Errorf("write upstream record: %w", err)
	}
	moon := "# Generated by fork-sync import; fill in the fork toolchain tasks before enabling CI.\nproject:\n  name: \"" + options.Name + "\"\n\n# The upstream tree is forks/" + options.Name + "/.\n"
	if err := os.WriteFile(filepath.Join(root, "forks", options.Name, "moon.yml"), []byte(moon), 0o644); err != nil {
		return fmt.Errorf("write moon project stub: %w", err)
	}
	if _, err := git(ctx, root, nil, "add", "forks/"+options.Name); err != nil {
		return fmt.Errorf("stage fork import: %w", err)
	}
	message := fmt.Sprintf("feat(forks): import %s at %s (upstream %s)", options.Name, short(forkCommit), short(base))
	commit := exec.CommandContext(ctx, "git", "commit", "-m", message) //nolint:gosec // Fixed command with generated message.
	commit.Dir = root
	commit.Env = commitIdentityEnv()
	if output, err := commit.CombinedOutput(); err != nil {
		return fmt.Errorf("commit fork import: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// forkRepoURL maps the RigelBuild/<repo> shorthand to its GitHub https URL. Any other value is returned unchanged for validateRepo to judge.
func forkRepoURL(fork string) string {
	if strings.HasPrefix(fork, "RigelBuild/") {
		return "https://github.com/" + fork + ".git"
	}
	return fork
}

func validateName(name string) error {
	if !safeName.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("invalid fork name %q", name)
	}
	return nil
}

func short(value string) string {
	if len(value) < 8 {
		return value
	}
	return value[:8]
}
