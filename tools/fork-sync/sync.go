package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/go-github/v93/github"
)

type forkConfig struct {
	Name        string
	Root        string
	BaseBranch  string
	GitHubToken string
	GitHubAPI   string
	Repository  string
	Outbox      string
	UpstreamTip string
	Client      *github.Client
	Output      io.Writer
}

type syncResult struct {
	Old, New, Commit string
	Conflict         bool
	PullRequest      int
}

func syncFork(ctx context.Context, config forkConfig, dryRun bool) error {
	if err := validateName(config.Name); err != nil {
		return err
	}
	recordPath := filepath.Join(config.Root, "forks", config.Name, "upstream.json")
	data, err := os.ReadFile(recordPath)
	if err != nil {
		return fmt.Errorf("read upstream record: %w", err)
	}
	var record upstreamRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return fmt.Errorf("decode upstream record: %w", err)
	}
	if record.Repo == "" || record.Branch == "" || record.Commit == "" {
		return errors.New("upstream record must contain repo, branch and commit")
	}
	if err := ensureClean(ctx, config.Root); err != nil {
		return fmt.Errorf("sync requires a clean worktree: %w", err)
	}
	if config.BaseBranch == "" {
		config.BaseBranch = "main"
	}
	temporary, err := os.MkdirTemp("", "nebula-fork-sync-")
	if err != nil {
		return fmt.Errorf("create temporary upstream repository: %w", err)
	}
	defer func() { _ = os.RemoveAll(temporary) }() // The temporary object store cannot affect the repository.
	if _, err := git(ctx, temporary, nil, "init", "--bare", "-q"); err != nil {
		return fmt.Errorf("initialize upstream repository: %w", err)
	}
	if _, err := git(ctx, temporary, nil, "fetch", "--quiet", "--depth=1", record.Repo, "refs/heads/"+record.Branch); err != nil {
		return fmt.Errorf("fetch upstream branch %s: %w", record.Branch, err)
	}
	tipBytes, err := git(ctx, temporary, nil, "rev-parse", "FETCH_HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("resolve upstream tip: %w", err)
	}
	tip := strings.TrimSpace(string(tipBytes))
	if config.UpstreamTip != "" {
		tip = config.UpstreamTip
	}
	if tip == record.Commit {
		fmt.Fprintf(syncOutput(config), "%s: up to date at %s\n", config.Name, short(tip))
		return nil
	}
	depth := 32
	for {
		if _, err := git(ctx, temporary, nil, "fetch", "--quiet", "--deepen", fmt.Sprint(depth), record.Repo, "refs/heads/"+record.Branch); err != nil {
			return fmt.Errorf("deepen upstream history: %w", err)
		}
		if _, err := git(ctx, temporary, nil, "cat-file", "-e", record.Commit+"^{commit}"); err == nil {
			break
		} else {
			shallow, shallowErr := git(ctx, temporary, nil, "rev-parse", "--is-shallow-repository")
			if shallowErr != nil {
				return fmt.Errorf("check shallow history: %w", shallowErr)
			}
			if strings.TrimSpace(string(shallow)) == "false" {
				return fmt.Errorf("recorded commit %s is not in upstream %s:%s history", record.Commit, record.Repo, record.Branch)
			}
			depth *= 2
		}
	}
	if _, err := git(ctx, temporary, nil, "merge-base", "--is-ancestor", record.Commit, tip); err != nil {
		return fmt.Errorf("recorded commit %s is not an ancestor of upstream tip %s: %w", short(record.Commit), short(tip), err)
	}
	commitCount := ""
	if dryRun {
		count, err := git(ctx, temporary, nil, "rev-list", "--count", record.Commit+".."+tip)
		if err != nil {
			return fmt.Errorf("count upstream commits: %w", err)
		}
		commitCount = strings.TrimSpace(string(count))
	}
	if dryRun {
		fmt.Fprintf(syncOutput(config), "%s: would open sync/%s (%s commits, %s..%s)\n", config.Name, config.Name, commitCount, short(record.Commit), short(tip))
		return nil
	}
	diff, err := git(ctx, temporary, nil, "diff", "--binary", "--find-renames", "--full-index", record.Commit, tip)
	if err != nil {
		return fmt.Errorf("create upstream diff: %w", err)
	}
	if err := transferPatchBlobs(ctx, temporary, config.Root, diff); err != nil {
		return fmt.Errorf("prepare three-way patch blobs: %w", err)
	}
	branch := "sync/" + config.Name
	remote := "origin"
	if config.Outbox != "" {
		remote = config.Outbox
	}
	if _, err := git(ctx, config.Root, nil, "fetch", "--quiet", remote, "refs/heads/main:refs/remotes/fork-sync/main"); err != nil {
		if gitExitCode(err) == 128 {
			if _, initErr := git(ctx, config.Root, nil, "update-ref", "refs/remotes/fork-sync/main", "HEAD"); initErr != nil {
				return fmt.Errorf("initialize fork-sync base ref: %w", initErr)
			}
		} else {
			return fmt.Errorf("fetch main branch: %w", err)
		}
	}
	if _, err := git(ctx, config.Root, nil, "fetch", "--quiet", remote, "refs/heads/"+branch+":refs/remotes/fork-sync/"+branch); err == nil {
		if _, err := git(ctx, config.Root, nil, "switch", "-C", branch, "refs/remotes/fork-sync/"+branch); err != nil {
			return fmt.Errorf("checkout existing sync branch: %w", err)
		}
	} else if gitExitCode(err) == 128 {
		if _, err := git(ctx, config.Root, nil, "switch", "-C", branch, "refs/remotes/fork-sync/main"); err != nil {
			return fmt.Errorf("create sync branch: %w", err)
		}
	} else {
		return fmt.Errorf("fetch existing sync branch: %w", err)
	}
	if len(diff) == 0 {
		return errors.New("upstream commit advanced but its tree diff is empty")
	}
	_, applyErr := git(ctx, config.Root, diff, "apply", "--3way", "--directory=forks/"+config.Name, "-")
	conflict := applyErr != nil
	if conflict {
		unmerged, statusErr := git(ctx, config.Root, nil, "diff", "--name-only", "--diff-filter=U")
		if statusErr != nil {
			return fmt.Errorf("check merge conflicts: %w (apply failed: %v)", statusErr, applyErr)
		}
		if len(unmerged) == 0 {
			return fmt.Errorf("apply upstream diff: %w", applyErr)
		}
	}
	old := record.Commit
	record.Commit = tip
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode upstream record: %w", err)
	}
	if err := os.WriteFile(recordPath, append(encoded, '\n'), 0o644); err != nil {
		return fmt.Errorf("update upstream record: %w", err)
	}
	if _, err := git(ctx, config.Root, nil, "add", "-A", "forks/"+config.Name); err != nil {
		return fmt.Errorf("stage upstream changes: %w", err)
	}
	commitMessage := fmt.Sprintf("sync(%s): %s..%s", config.Name, short(old), short(tip))
	commit := exec.CommandContext(ctx, "git", "commit", "-m", commitMessage) //nolint:gosec // Fixed subcommand and generated message.
	commit.Dir = config.Root
	commit.Env = commitIdentityEnv()
	if output, err := commit.CombinedOutput(); err != nil {
		return fmt.Errorf("commit upstream sync: %w: %s", err, strings.TrimSpace(string(output)))
	}
	headBytes, err := git(ctx, config.Root, nil, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("resolve sync commit: %w", err)
	}
	result := syncResult{Old: old, New: tip, Commit: strings.TrimSpace(string(headBytes)), Conflict: conflict}
	if _, err := git(ctx, config.Root, nil, "push", "--force-with-lease", remote, "HEAD:refs/heads/"+branch); err != nil {
		return fmt.Errorf("push sync branch: %w", err)
	}
	if config.Client == nil {
		return errors.New("GitHub client is not configured")
	}
	if err := updatePullRequest(ctx, config, branch, &result); err != nil {
		return err
	}
	return nil
}

func syncOutput(config forkConfig) io.Writer {
	if config.Output != nil {
		return config.Output
	}
	return os.Stdout
}

func updatePullRequest(ctx context.Context, config forkConfig, branch string, result *syncResult) error {
	owner, repository, ok := strings.Cut(config.Repository, "/")
	if !ok || owner == "" || repository == "" {
		return fmt.Errorf("invalid GitHub repository %q", config.Repository)
	}
	title := fmt.Sprintf("sync(%s): %s..%s", config.Name, short(result.Old), short(result.New))
	body := fmt.Sprintf("Sync upstream %s from %s to %s.\n", config.Name, result.Old, result.New)
	pulls, _, err := config.Client.PullRequests.List(ctx, owner, repository, &github.PullRequestListOptions{State: "open", Head: owner + ":" + branch, Base: config.BaseBranch})
	if err != nil {
		return fmt.Errorf("list sync pull requests: %w", err)
	}
	if len(pulls) == 0 {
		created, _, createErr := config.Client.PullRequests.Create(ctx, owner, repository, github.CreatePullRequest{Title: &title, Head: owner + ":" + branch, Base: config.BaseBranch, Body: &body})
		if createErr != nil {
			return fmt.Errorf("create sync pull request: %w", createErr)
		}
		result.PullRequest = created.GetNumber()
	} else {
		result.PullRequest = pulls[0].GetNumber()
		_, _, err = config.Client.PullRequests.Edit(ctx, owner, repository, result.PullRequest, &github.PullRequest{Title: &title, Body: &body})
		if err != nil {
			return fmt.Errorf("update sync pull request: %w", err)
		}
	}
	if result.Conflict {
		if _, _, err := config.Client.Issues.AddLabelsToIssue(ctx, owner, repository, result.PullRequest, []string{"conflict"}); err != nil {
			return fmt.Errorf("label sync pull request conflict: %w", err)
		}
	} else if _, err := config.Client.Issues.RemoveLabelForIssue(ctx, owner, repository, result.PullRequest, "conflict"); err != nil && !isGitHubNotFound(err) {
		return fmt.Errorf("remove stale conflict label: %w", err)
	}
	return nil
}

func isGitHubNotFound(err error) bool {
	var response *github.ErrorResponse
	return errors.As(err, &response) && response.Response != nil && response.Response.StatusCode == 404
}

func listForks(root string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(root, "forks", "*", "upstream.json"))
	if err != nil {
		return nil, fmt.Errorf("find upstream records: %w", err)
	}
	names := make([]string, 0, len(matches))
	for _, match := range matches {
		name := filepath.Base(filepath.Dir(match))
		if err := validateName(name); err != nil {
			return nil, fmt.Errorf("invalid upstream record file %q: %w", match, err)
		}
		names = append(names, name)
	}
	return names, nil
}
