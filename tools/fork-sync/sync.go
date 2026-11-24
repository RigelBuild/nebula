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
	"strconv"
	"strings"

	"github.com/google/go-github/v93/github"
)

type forkConfig struct {
	Name        string
	Root        string
	BaseBranch  string
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
	record, err := readUpstreamRecord(recordPath)
	if err != nil {
		return err
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
	if _, err := git(ctx, temporary, nil, "fetch", "--quiet", "--depth=1", "--", record.Repo, "refs/heads/"+record.Branch); err != nil {
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
		if _, err := git(ctx, temporary, nil, "fetch", "--quiet", "--deepen", fmt.Sprint(depth), "--", record.Repo, "refs/heads/"+record.Branch); err != nil {
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
		if err := unshallowUpstream(ctx, temporary, record.Repo, record.Branch); err != nil {
			return fmt.Errorf("fetch full upstream history: %w", err)
		}
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
	rawDiff, err := git(ctx, temporary, nil, "diff", "--raw", "-z", "--find-renames", "--full-index", record.Commit, tip)
	if err != nil {
		return fmt.Errorf("create upstream raw diff: %w", err)
	}
	entries, err := parseRawDiff(rawDiff)
	if err != nil {
		return fmt.Errorf("parse upstream raw diff: %w", err)
	}
	diff, err := git(ctx, temporary, nil, "diff", "--binary", "--find-renames", "--full-index", record.Commit, tip)
	if err != nil {
		return fmt.Errorf("create upstream diff: %w", err)
	}
	diff, skipped, err := filterGitlinkDiff(diff, entries)
	if err != nil {
		return fmt.Errorf("filter upstream diff: %w", err)
	}
	for _, path := range skipped {
		warnGitlink(path)
	}
	if err := transferPatchBlobs(ctx, temporary, config.Root, diff); err != nil {
		return fmt.Errorf("prepare three-way patch blobs: %w", err)
	}
	branch := "sync/" + config.Name
	remote := "origin"
	if config.Outbox != "" {
		remote = config.Outbox
	}
	if _, err := git(ctx, config.Root, nil, "fetch", "--quiet", "--", remote, "refs/heads/main:refs/remotes/fork-sync/main"); err != nil {
		if gitExitCode(err) == 128 {
			if _, initErr := git(ctx, config.Root, nil, "update-ref", "refs/remotes/fork-sync/main", "HEAD"); initErr != nil {
				return fmt.Errorf("initialize fork-sync base ref: %w", initErr)
			}
		} else {
			return fmt.Errorf("fetch main branch: %w", err)
		}
	}
	leaseSHA := ""
	if _, err := git(ctx, config.Root, nil, "fetch", "--quiet", "--", remote, "refs/heads/"+branch+":refs/remotes/fork-sync/"+branch); err == nil {
		fetched, revErr := git(ctx, config.Root, nil, "rev-parse", "refs/remotes/fork-sync/"+branch)
		if revErr != nil {
			return fmt.Errorf("resolve existing sync branch: %w", revErr)
		}
		leaseSHA = strings.TrimSpace(string(fetched))
		if _, err := git(ctx, config.Root, nil, "switch", "-C", branch, "refs/remotes/fork-sync/"+branch); err != nil {
			return fmt.Errorf("checkout existing sync branch: %w", err)
		}
		// A sync PR from an earlier run already records this tip. Applying the diff again would
		// produce no change, so the run stops here and leaves the PR as it is.
		if onBranch, readErr := readUpstreamRecord(recordPath); readErr == nil && onBranch.Commit == tip {
			fmt.Fprintf(syncOutput(config), "%s: sync branch already at %s\n", config.Name, short(tip))
			return nil
		}
	} else if gitExitCode(err) == 128 {
		if _, err := git(ctx, config.Root, nil, "switch", "-C", branch, "refs/remotes/fork-sync/main"); err != nil {
			return fmt.Errorf("create sync branch: %w", err)
		}
	} else {
		return fmt.Errorf("fetch existing sync branch: %w", err)
	}
	if len(diff) == 0 && len(entries) == 0 {
		return errors.New("upstream commit advanced but its tree diff is empty")
	}
	var applyErr error
	if len(diff) > 0 {
		_, applyErr = git(ctx, config.Root, diff, "apply", "--3way", "--directory=forks/"+config.Name, "-")
	}
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
	lease := "--force-with-lease=refs/heads/" + branch + ":" + leaseSHA
	if _, err := git(ctx, config.Root, nil, "push", lease, "--", remote, "HEAD:refs/heads/"+branch); err != nil {
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

type rawDiffEntry struct {
	sourcePath, destinationPath string
	sourceMode, destinationMode string
	status                      byte
}

func parseRawDiff(raw []byte) ([]rawDiffEntry, error) {
	fields := bytes.Split(raw, []byte{0})
	entries := make([]rawDiffEntry, 0)
	for i := 0; i < len(fields) && len(fields[i]) > 0; {
		metadata := strings.Fields(string(fields[i]))
		if len(metadata) != 5 || !strings.HasPrefix(metadata[0], ":") || metadata[4] == "" {
			return nil, fmt.Errorf("parse raw diff metadata %q", fields[i])
		}
		entry := rawDiffEntry{
			sourceMode:      metadata[0][1:],
			destinationMode: metadata[1],
			status:          metadata[4][0],
		}
		i++
		if i >= len(fields) {
			return nil, errors.New("raw diff is missing path")
		}
		entry.sourcePath = string(fields[i])
		i++
		entry.destinationPath = entry.sourcePath
		if entry.status == 'R' || entry.status == 'C' {
			if i >= len(fields) {
				return nil, errors.New("raw rename diff is missing destination path")
			}
			entry.destinationPath = string(fields[i])
			i++
		}
		if entry.status == 'T' {
			deleted := entry
			deleted.destinationMode = "000000"
			added := entry
			added.sourceMode = "000000"
			entries = append(entries, deleted, added)
			continue
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func filterGitlinkDiff(patch []byte, entries []rawDiffEntry) ([]byte, []string, error) {
	var filtered bytes.Buffer
	skipped := make([]string, 0)
	skippedSet := make(map[string]struct{})
	offset := 0
	for len(entries) > 0 {
		start := bytes.Index(patch[offset:], []byte("diff --git "))
		if start < 0 {
			return nil, nil, fmt.Errorf("raw diff entry for %q has no patch section", entries[0].sourcePath)
		}
		start += offset
		filtered.Write(patch[offset:start])
		remainder := patch[start+len("diff --git "):]
		next := bytes.Index(remainder, []byte("\ndiff --git "))
		end := len(patch)
		if next >= 0 {
			end = start + len("diff --git ") + next + 1
		}
		section := patch[start:end]
		match := -1
		for i, candidate := range entries {
			if !diffHeaderMatches(section, candidate) {
				continue
			}
			if bytes.Contains(section, []byte("\ndeleted file mode ")) && candidate.destinationMode != "000000" {
				continue
			}
			if bytes.Contains(section, []byte("\nnew file mode ")) && candidate.sourceMode != "000000" {
				continue
			}
			match = i
			break
		}
		if match < 0 {
			return nil, nil, fmt.Errorf("patch section has no matching raw diff entry: %q", bytes.SplitN(section, []byte{'\n'}, 2)[0])
		}
		entry := entries[match]
		entries = append(entries[:match], entries[match+1:]...)
		if entry.sourceMode == "160000" || entry.destinationMode == "160000" {
			path := entry.sourcePath
			if entry.destinationMode == "160000" {
				path = entry.destinationPath
			}
			if _, exists := skippedSet[path]; !exists {
				skippedSet[path] = struct{}{}
				skipped = append(skipped, path)
			}
			offset = end
			continue
		}
		filtered.Write(section)
		offset = end
	}
	if bytes.Index(patch[offset:], []byte("diff --git ")) >= 0 {
		return nil, nil, errors.New("patch has more sections than raw diff")
	}
	filtered.Write(patch[offset:])
	return filtered.Bytes(), skipped, nil
}

func diffHeaderMatches(section []byte, entry rawDiffEntry) bool {
	lineEnd := bytes.IndexByte(section, '\n')
	if lineEnd < 0 || !bytes.HasPrefix(section, []byte("diff --git ")) {
		return false
	}
	header := section[len("diff --git "):lineEnd]
	oldPath, rest, ok := parseDiffPathToken(header, "a/"+entry.sourcePath)
	if !ok || oldPath != "a/"+entry.sourcePath || len(rest) == 0 || rest[0] != ' ' {
		return false
	}
	newPath, rest, ok := parseDiffPathToken(rest[1:], "b/"+entry.destinationPath)
	return ok && newPath == "b/"+entry.destinationPath && len(rest) == 0
}

func parseDiffPathToken(input []byte, expected string) (string, []byte, bool) {
	if len(input) == 0 {
		return "", nil, false
	}
	if input[0] != '"' {
		if !bytes.HasPrefix(input, []byte(expected)) {
			return "", nil, false
		}
		return expected, input[len(expected):], true
	}
	for i := 1; i < len(input); i++ {
		if input[i] == '\\' {
			i++
			continue
		}
		if input[i] == '"' {
			path, err := strconv.Unquote(string(input[:i+1]))
			if err != nil {
				return "", nil, false
			}
			return path, input[i+1:], true
		}
	}
	return "", nil, false
}

// unshallowUpstream fetches the rest of the upstream history when the temporary repository is shallow. A shallow
// fetch can stop before a merge's other parent, so a commit count taken there is wrong. The blob filter keeps the
// fetch to commits and trees, which is all a count reads.
func unshallowUpstream(ctx context.Context, repository, upstream, branch string) error {
	shallow, err := git(ctx, repository, nil, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return fmt.Errorf("check shallow history: %w", err)
	}
	if strings.TrimSpace(string(shallow)) != "true" {
		return nil
	}
	_, err = git(ctx, repository, nil, "fetch", "--quiet", "--unshallow", "--filter=blob:none", "--", upstream, "refs/heads/"+branch)
	return err
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
