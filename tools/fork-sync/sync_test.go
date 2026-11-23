package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-github/v93/github"
)

func TestSyncSkipsChangedGitlink(t *testing.T) {
	fixture := newSyncFixture(t, false)
	prepareSyncGitlinkChange(t, fixture, "0123456789abcdef0123456789abcdef01234567", "1123456789abcdef0123456789abcdef01234567")
	assertSyncSkipsGitlink(t, fixture)
}

func TestSyncSkipsAddedGitlink(t *testing.T) {
	fixture := newSyncFixture(t, false)
	prepareSyncGitlinkChange(t, fixture, "", "0123456789abcdef0123456789abcdef01234567")
	assertSyncSkipsGitlink(t, fixture)
}

func TestSyncSkipsDeletedGitlink(t *testing.T) {
	fixture := newSyncFixture(t, false)
	prepareSyncGitlinkChange(t, fixture, "0123456789abcdef0123456789abcdef01234567", "")
	assertSyncSkipsGitlink(t, fixture)
}

func TestSyncReplacesGitlinkWithDirectory(t *testing.T) {
	fixture := newSyncFixture(t, false)
	prepareSyncGitlinkReplacement(t, fixture, "sub/child.txt", "directory content\n")
	assertSyncGitlinkReplacement(t, fixture, "sub/child.txt", "directory content")
}

func TestSyncReplacesGitlinkWithFile(t *testing.T) {
	fixture := newSyncFixture(t, false)
	prepareSyncGitlinkReplacement(t, fixture, "sub", "file content\n")
	assertSyncGitlinkReplacement(t, fixture, "sub", "file content")
}

func TestSyncSkipsRenamedGitlink(t *testing.T) {
	fixture := newSyncFixture(t, false)
	prepareSyncGitlinkRename(t, fixture, "sub", "renamed")
	assertSyncSkipsGitlinkPath(t, fixture, "renamed")
}

func TestSyncSkipsNonASCIIPathGitlink(t *testing.T) {
	fixture := newSyncFixture(t, false)
	prepareSyncGitlinkAtPath(t, fixture, "café")
	assertSyncSkipsGitlinkPath(t, fixture, "café")
}

func TestDiffHeaderMatchesQuotedPaths(t *testing.T) {
	entry := rawDiffEntry{sourcePath: "café\tfile", destinationPath: "café\tfile"}
	section := []byte("diff --git \"a/caf\\303\\251\\tfile\" \"b/caf\\303\\251\\tfile\"\n")
	if !diffHeaderMatches(section, entry) {
		t.Fatal("quoted Git paths did not match raw paths")
	}
}

func prepareSyncGitlinkReplacement(t *testing.T, fixture *syncFixture, path, content string) {
	t.Helper()
	gitTest(t, fixture.upstream, "read-tree", fixture.oldCommit)
	gitTest(t, fixture.upstream, "update-index", "--add", "--cacheinfo", "160000,0123456789abcdef0123456789abcdef01234567,sub")
	gitlinksTree := strings.TrimSpace(string(gitOutput(t, fixture.upstream, "write-tree")))
	gitlinkCommit := strings.TrimSpace(string(gitOutput(t, fixture.upstream, "commit-tree", gitlinksTree, "-p", fixture.oldCommit, "-m", "recorded gitlink")))
	fixture.oldCommit = gitlinkCommit
	gitTest(t, fixture.upstream, "read-tree", gitlinkCommit)
	gitTest(t, fixture.upstream, "update-index", "--force-remove", "sub")
	blob := hashRaw(t, fixture.upstream, content)
	gitTest(t, fixture.upstream, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+path)
	valueBlob := hashRaw(t, fixture.upstream, "upstream\n")
	gitTest(t, fixture.upstream, "update-index", "--cacheinfo", "100644,"+valueBlob+",value.txt")
	newTree := strings.TrimSpace(string(gitOutput(t, fixture.upstream, "write-tree")))
	fixture.newCommit = strings.TrimSpace(string(gitOutput(t, fixture.upstream, "commit-tree", newTree, "-p", gitlinkCommit, "-m", "replace gitlink and update value")))
	gitTest(t, fixture.upstream, "update-ref", "refs/heads/main", fixture.newCommit)
	fixture.writeRecord(t, gitlinkCommit)
	gitTest(t, fixture.root, "add", "forks/widget/upstream.json")
	gitTest(t, fixture.root, "commit", "-m", "update recorded upstream")
	gitTest(t, fixture.root, "push", "origin", "main")
}

func assertSyncGitlinkReplacement(t *testing.T, fixture *syncFixture, path, content string) {
	t.Helper()
	server := fixture.github(t)
	defer server.Close()
	fixture.config.Client = newGitHubClient(t, server.URL)
	log := captureSlog(t, func() {
		if err := syncFork(context.Background(), fixture.config, false); err != nil {
			t.Fatal(err)
		}
	})
	assertGitlinkWarning(t, log, "sub")
	if got := strings.TrimSpace(string(mustRead(t, filepath.Join(fixture.root, "forks/widget", path)))); got != content {
		t.Fatalf("replacement %s = %q, want %q", path, got, content)
	}
	if got := strings.TrimSpace(string(mustRead(t, filepath.Join(fixture.root, "forks/widget/value.txt")))); got != "upstream" {
		t.Fatalf("synced value.txt = %q, want upstream", got)
	}
}

func assertSyncSkipsGitlinkPath(t *testing.T, fixture *syncFixture, path string) {
	t.Helper()
	server := fixture.github(t)
	defer server.Close()
	fixture.config.Client = newGitHubClient(t, server.URL)
	log := captureSlog(t, func() {
		if err := syncFork(context.Background(), fixture.config, false); err != nil {
			t.Fatal(err)
		}
	})
	assertGitlinkWarning(t, log, path)
	if got := strings.TrimSpace(string(mustRead(t, filepath.Join(fixture.root, "forks/widget/value.txt")))); got != "upstream" {
		t.Fatalf("synced value.txt = %q, want upstream", got)
	}
}

func prepareSyncGitlinkAtPath(t *testing.T, fixture *syncFixture, path string) {
	t.Helper()
	gitTest(t, fixture.upstream, "read-tree", fixture.oldCommit)
	gitTest(t, fixture.upstream, "update-index", "--add", "--cacheinfo", "160000,0123456789abcdef0123456789abcdef01234567,"+path)
	valueBlob := hashRaw(t, fixture.upstream, "upstream\n")
	gitTest(t, fixture.upstream, "update-index", "--cacheinfo", "100644,"+valueBlob+",value.txt")
	newTree := strings.TrimSpace(string(gitOutput(t, fixture.upstream, "write-tree")))
	fixture.newCommit = strings.TrimSpace(string(gitOutput(t, fixture.upstream, "commit-tree", newTree, "-p", fixture.oldCommit, "-m", "add gitlink")))
	gitTest(t, fixture.upstream, "update-ref", "refs/heads/main", fixture.newCommit)
}
func prepareSyncGitlinkRename(t *testing.T, fixture *syncFixture, oldPath, newPath string) {
	t.Helper()

	gitTest(t, fixture.upstream, "read-tree", fixture.oldCommit)
	gitTest(t, fixture.upstream, "update-index", "--add", "--cacheinfo", "160000,0123456789abcdef0123456789abcdef01234567,"+oldPath)
	oldTree := strings.TrimSpace(string(gitOutput(t, fixture.upstream, "write-tree")))
	fixture.oldCommit = strings.TrimSpace(string(gitOutput(t, fixture.upstream, "commit-tree", oldTree, "-p", fixture.oldCommit, "-m", "add old gitlink")))
	gitTest(t, fixture.upstream, "read-tree", fixture.oldCommit)
	gitTest(t, fixture.upstream, "update-index", "--force-remove", oldPath)
	gitTest(t, fixture.upstream, "update-index", "--add", "--cacheinfo", "160000,0123456789abcdef0123456789abcdef01234567,"+newPath)
	valueBlob := hashRaw(t, fixture.upstream, "upstream\n")
	gitTest(t, fixture.upstream, "update-index", "--cacheinfo", "100644,"+valueBlob+",value.txt")
	newTree := strings.TrimSpace(string(gitOutput(t, fixture.upstream, "write-tree")))
	fixture.newCommit = strings.TrimSpace(string(gitOutput(t, fixture.upstream, "commit-tree", newTree, "-p", fixture.oldCommit, "-m", "rename gitlink")))
	gitTest(t, fixture.upstream, "update-ref", "refs/heads/main", fixture.newCommit)
	fixture.writeRecord(t, fixture.oldCommit)
	gitTest(t, fixture.root, "add", "forks/widget/upstream.json")
	gitTest(t, fixture.root, "commit", "-m", "update recorded upstream")
	gitTest(t, fixture.root, "push", "origin", "main")
}

func prepareSyncGitlinkChange(t *testing.T, fixture *syncFixture, oldObject, newObject string) {
	t.Helper()
	gitTest(t, fixture.upstream, "read-tree", fixture.oldCommit)
	if oldObject != "" {
		gitTest(t, fixture.upstream, "update-index", "--add", "--cacheinfo", "160000,"+oldObject+",sub")
		oldTree := strings.TrimSpace(string(gitOutput(t, fixture.upstream, "write-tree")))
		fixture.oldCommit = strings.TrimSpace(string(gitOutput(t, fixture.upstream, "commit-tree", oldTree, "-p", fixture.oldCommit, "-m", "recorded gitlink")))
		gitTest(t, fixture.upstream, "read-tree", fixture.oldCommit)
	}
	if newObject == "" {
		gitTest(t, fixture.upstream, "update-index", "--force-remove", "sub")
	} else {
		gitTest(t, fixture.upstream, "update-index", "--add", "--cacheinfo", "160000,"+newObject+",sub")
	}
	valueBlob := hashRaw(t, fixture.upstream, "upstream\n")
	gitTest(t, fixture.upstream, "update-index", "--cacheinfo", "100644,"+valueBlob+",value.txt")
	newTree := strings.TrimSpace(string(gitOutput(t, fixture.upstream, "write-tree")))
	fixture.newCommit = strings.TrimSpace(string(gitOutput(t, fixture.upstream, "commit-tree", newTree, "-p", fixture.oldCommit, "-m", "update gitlink and value")))
	gitTest(t, fixture.upstream, "update-ref", "refs/heads/main", fixture.newCommit)
	if oldObject != "" {
		fixture.writeRecord(t, fixture.oldCommit)
		gitTest(t, fixture.root, "add", "forks/widget/upstream.json")
		gitTest(t, fixture.root, "commit", "-m", "update recorded upstream")
		gitTest(t, fixture.root, "push", "origin", "main")
	}
}

func assertSyncSkipsGitlink(t *testing.T, fixture *syncFixture) {
	t.Helper()
	server := fixture.github(t)
	defer server.Close()
	fixture.config.Client = newGitHubClient(t, server.URL)
	log := captureSlog(t, func() {
		if err := syncFork(context.Background(), fixture.config, false); err != nil {
			t.Fatal(err)
		}
	})
	assertGitlinkWarning(t, log, "sub")
	if got := strings.TrimSpace(string(mustRead(t, filepath.Join(fixture.root, "forks/widget/value.txt")))); got != "upstream" {
		t.Fatalf("synced value.txt = %q, want upstream", got)
	}
	if mode := commitMode(t, fixture.root, "forks/widget/sub"); mode != "" {
		t.Fatalf("gitlink was added to sync commit with mode %s", mode)
	}
}

func TestSyncUpdatesExistingPR(t *testing.T) {
	fixture := newSyncFixture(t, false)
	fixture.existingPR = true
	server := fixture.github(t)
	defer server.Close()
	fixture.config.Client = newGitHubClient(t, server.URL)
	if err := syncFork(context.Background(), fixture.config, false); err != nil {
		t.Fatal(err)
	}
	if fixture.createdPRs != 0 || fixture.updatedPRs != 1 {
		t.Fatalf("PR operations = create %d, update %d; want create 0, update 1", fixture.createdPRs, fixture.updatedPRs)
	}
	want := "sync(widget): " + short(fixture.oldCommit) + ".." + short(fixture.newCommit)
	if fixture.title != want {
		t.Fatalf("updated PR title = %q, want %q", fixture.title, want)
	}
}

func TestSyncCreatesPR(t *testing.T) {
	fixture := newSyncFixture(t, false)
	server := fixture.github(t)
	defer server.Close()
	fixture.config.Client = newGitHubClient(t, server.URL)
	if err := syncFork(context.Background(), fixture.config, false); err != nil {
		t.Fatal(err)
	}
	if got := fixture.recordedCommit(t); got != fixture.newCommit {
		t.Fatalf("recorded commit = %s, want %s", got, fixture.newCommit)
	}
	if want := "sync(widget): " + short(fixture.oldCommit) + ".." + short(fixture.newCommit); fixture.title != want {
		t.Fatalf("PR title = %q, want %q", fixture.title, want)
	}
	if fixture.branch != "RigelBuild:sync/widget" {
		t.Fatalf("PR branch = %q", fixture.branch)
	}
}

func TestSyncConflictLabelsPR(t *testing.T) {
	fixture := newSyncFixture(t, true)
	server := fixture.github(t)
	defer server.Close()
	fixture.config.Client = newGitHubClient(t, server.URL)
	if err := syncFork(context.Background(), fixture.config, false); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(fixture.root, "forks/widget/value.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "<<<<<<<") || fixture.label != "conflict" {
		t.Fatalf("conflict markers or label missing: content=%q label=%q", content, fixture.label)
	}
}

func TestSyncNoopPrintsPlan(t *testing.T) {
	for _, test := range []struct {
		name   string
		dryRun bool
	}{
		{name: "dry-run", dryRun: true},
		{name: "sync"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSyncFixture(t, false)
			fixture.config.UpstreamTip = fixture.oldCommit
			var output strings.Builder
			fixture.config.Output = &output
			if err := syncFork(context.Background(), fixture.config, test.dryRun); err != nil {
				t.Fatal(err)
			}
			want := "widget: up to date at " + short(fixture.oldCommit) + "\n"
			if got := output.String(); got != want {
				t.Fatalf("sync output = %q, want %q", got, want)
			}
		})
	}
}

func TestSyncDryRunPrintsUpdatePlan(t *testing.T) {
	fixture := newSyncFixture(t, false)
	var output strings.Builder
	if err := syncFork(context.Background(), forkConfig{
		Name:        "widget",
		Root:        fixture.root,
		Repository:  "RigelBuild/nebula",
		UpstreamTip: fixture.newCommit,
		Output:      &output,
	}, true); err != nil {
		t.Fatal(err)
	}
	want := "widget: would open sync/widget (1 commits, " + short(fixture.oldCommit) + ".." + short(fixture.newCommit) + ")\n"
	if got := output.String(); got != want {
		t.Fatalf("dry-run output = %q, want %q", got, want)
	}
}

// TestSyncDryRunCountsFullHistory: the dry-run count must equal git rev-list --count on the full upstream history.
// The side branch forks at the recorded base and merges after it. A shallow fetch that stops once the base is
// present keeps only part of the side branch, so a shallow count comes out short.
func TestSyncDryRunCountsFullHistory(t *testing.T) {
	fixture := newSyncFixture(t, false)
	tree := strings.TrimSpace(string(gitOutput(t, fixture.upstream, "rev-parse", fixture.oldCommit+"^{tree}")))
	side := fixture.oldCommit
	for i := range 40 {
		side = strings.TrimSpace(string(gitOutput(t, fixture.upstream, "commit-tree", tree, "-p", side, "-m", "side "+strconv.Itoa(i))))
	}
	upstreamTree := strings.TrimSpace(string(gitOutput(t, fixture.upstream, "rev-parse", fixture.newCommit+"^{tree}")))
	merged := strings.TrimSpace(string(gitOutput(t, fixture.upstream, "commit-tree", upstreamTree, "-p", fixture.newCommit, "-p", side, "-m", "merge side")))
	gitTest(t, fixture.upstream, "update-ref", "refs/heads/main", merged)
	full := strings.TrimSpace(string(gitOutput(t, fixture.upstream, "rev-list", "--count", fixture.oldCommit+".."+merged)))
	var output strings.Builder
	if err := syncFork(context.Background(), forkConfig{
		Name:       "widget",
		Root:       fixture.root,
		Repository: "RigelBuild/nebula",
		Output:     &output,
	}, true); err != nil {
		t.Fatal(err)
	}
	want := "widget: would open sync/widget (" + full + " commits, " + short(fixture.oldCommit) + ".." + short(merged) + ")\n"
	if got := output.String(); got != want {
		t.Fatalf("dry-run output = %q, want %q", got, want)
	}
}

func TestSyncUnchangedTipDoesNotMutate(t *testing.T) {
	fixture := newSyncFixture(t, false)
	fixture.config.UpstreamTip = fixture.oldCommit
	server := fixture.github(t)
	defer server.Close()
	fixture.config.Client = newGitHubClient(t, server.URL)
	if err := syncFork(context.Background(), fixture.config, false); err != nil {
		t.Fatal(err)
	}
	if fixture.title != "" {
		t.Fatalf("created pull request for unchanged upstream tip: %q", fixture.title)
	}
	if got := fixture.recordedCommit(t); got != fixture.oldCommit {
		t.Fatalf("recorded commit changed to %s", got)
	}
}

// TestSyncSecondRunAdvancesSyncBranch syncs, advances upstream, and syncs again onto the existing
// sync/<name> branch. The second push must succeed and carry the newest upstream change.
func TestSyncSecondRunAdvancesSyncBranch(t *testing.T) {
	fixture := newSyncFixture(t, false)
	server := fixture.github(t)
	defer server.Close()
	fixture.config.Client = newGitHubClient(t, server.URL)
	if err := syncFork(context.Background(), fixture.config, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	first := outboxBranch(t, fixture.outbox, "sync/widget")
	next := fixture.advanceUpstream(t)
	fixture.existingPR = true
	if err := syncFork(context.Background(), fixture.config, false); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	second := outboxBranch(t, fixture.outbox, "sync/widget")
	if second == first {
		t.Fatalf("second sync left sync/widget at %s", short(first))
	}
	if got := strings.TrimSpace(string(gitOutput(t, fixture.outbox, "show", second+":forks/widget/value.txt"))); got != "third" {
		t.Fatalf("sync/widget value.txt = %q, want %q", got, "third")
	}
	if got := fixture.recordedCommit(t); got != next {
		t.Fatalf("recorded commit = %s, want %s", got, next)
	}
	if fixture.createdPRs != 1 || fixture.updatedPRs != 1 {
		t.Fatalf("PR operations = create %d, update %d; want create 1, update 1", fixture.createdPRs, fixture.updatedPRs)
	}
}

// TestSyncRepeatAtSameTipIsNoop syncs once, then syncs again at the same upstream tip while the sync PR
// is still open. The second run must succeed without a new commit or a PR write.
func TestSyncRepeatAtSameTipIsNoop(t *testing.T) {
	fixture := newSyncFixture(t, false)
	server := fixture.github(t)
	defer server.Close()
	fixture.config.Client = newGitHubClient(t, server.URL)
	if err := syncFork(context.Background(), fixture.config, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// The hourly job starts from a fresh checkout of main, whose upstream.json still records the
	// old commit. Reset the worktree so the second run reads that record, not the one the first run
	// left on sync/<name>.
	gitTest(t, fixture.root, "switch", "main")
	first := outboxBranch(t, fixture.outbox, "sync/widget")
	fixture.existingPR = true
	if err := syncFork(context.Background(), fixture.config, false); err != nil {
		t.Fatalf("second sync at the same tip: %v", err)
	}
	if second := outboxBranch(t, fixture.outbox, "sync/widget"); second != first {
		t.Fatalf("second sync moved sync/widget from %s to %s", short(first), short(second))
	}
	if fixture.createdPRs != 1 || fixture.updatedPRs != 0 {
		t.Fatalf("PR operations = create %d, update %d; want create 1, update 0", fixture.createdPRs, fixture.updatedPRs)
	}
}

// TestSyncRefusesOptionRecordRepo: a record repo that git would read as an option is refused before any fetch.
func TestSyncRefusesOptionRecordRepo(t *testing.T) {
	fixture := newSyncFixture(t, false)
	marker := filepath.Join(t.TempDir(), "marker")
	fixture.commitRecordRepo(t, "--upload-pack=touch "+marker, fixture.oldCommit)
	err := syncFork(context.Background(), fixture.config, true)
	requireRefusedRepo(t, err, marker)
}

// TestSyncOutboxIsNotReadAsOption: a dash-leading outbox must reach the fetch and push as a repository operand after
// "--". Without "--", git reads it as an option and fails with "unknown option"; with it, git refuses it as a path.
func TestSyncOutboxIsNotReadAsOption(t *testing.T) {
	fixture := newSyncFixture(t, false)
	fixture.config.Outbox = "--fork-sync-test-option"
	err := syncFork(context.Background(), fixture.config, false)
	if err == nil {
		t.Fatal("sync with a dash-leading outbox succeeded, want git to refuse it")
	}
	if strings.Contains(err.Error(), "unknown option") {
		t.Fatalf("git read the outbox as an option: %v", err)
	}
}

// TestSyncAcceptsSupportedRecordRepos: supported schemes pass validation and reach the fetch, which fails
// because the port is closed. The error must not be a refusal.
func TestSyncAcceptsSupportedRecordRepos(t *testing.T) {
	for _, test := range []struct {
		name, repo string
	}{
		{name: "https", repo: "https://127.0.0.1:1/widget.git"},
		{name: "ssh", repo: "ssh://127.0.0.1:1/widget.git"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSyncFixture(t, false)
			fixture.commitRecordRepo(t, test.repo, fixture.oldCommit)
			err := syncFork(context.Background(), fixture.config, true)
			if err != nil && strings.Contains(err.Error(), "refused upstream repo") {
				t.Fatalf("supported record repo refused: %v", err)
			}
		})
	}
}

type syncFixture struct {
	root, upstream, repo, newCommit, oldCommit string
	outbox                                     string
	config                                     forkConfig
	title, branch, label                       string
	existingPR                                 bool
	createdPRs, updatedPRs                     int
}

func newSyncFixture(t *testing.T, conflict bool) *syncFixture {
	t.Helper()
	root := t.TempDir()
	upstream := filepath.Join(t.TempDir(), "upstream")
	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	gitTest(t, upstream, "init", "-b", "main")
	gitTest(t, upstream, "config", "user.name", "Test")
	gitTest(t, upstream, "config", "user.email", "test@example.com")
	writeTestFile(t, upstream, "value.txt", "base\n")
	gitTest(t, upstream, "add", ".")
	gitTest(t, upstream, "commit", "-m", "base")
	old := strings.TrimSpace(string(gitOutput(t, upstream, "rev-parse", "HEAD")))
	writeTestFile(t, upstream, "value.txt", "upstream\n")
	gitTest(t, upstream, "commit", "-am", "upstream change")
	newCommit := strings.TrimSpace(string(gitOutput(t, upstream, "rev-parse", "HEAD")))
	if err := os.MkdirAll(filepath.Join(root, "forks/widget"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "base\n"
	if conflict {
		content = "local change\n"
	}
	writeTestFile(t, root, "forks/widget/value.txt", content)
	gitTest(t, root, "init", "-b", "main")
	gitTest(t, root, "config", "user.name", "Test")
	gitTest(t, root, "config", "user.email", "test@example.com")
	mapRemotes(t, map[string]string{"upstream": upstream})
	fixture := &syncFixture{
		root: root, upstream: upstream, repo: "https://fixture.invalid/upstream",
		oldCommit: old, newCommit: newCommit,
		config: forkConfig{Name: "widget", Root: root, BaseBranch: "main", Repository: "RigelBuild/nebula"},
	}
	fixture.writeRecord(t, old)
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-m", "initial")
	outbox := filepath.Join(t.TempDir(), "outbox.git")
	gitTest(t, t.TempDir(), "init", "--bare", "-b", "main", outbox)
	gitTest(t, root, "remote", "add", "origin", outbox)
	gitTest(t, root, "push", "origin", "main")
	fixture.config.Outbox = outbox
	fixture.outbox = outbox
	return fixture
}

func (fixture *syncFixture) writeRecord(t *testing.T, commit string) {
	t.Helper()
	encoded, err := json.Marshal(upstreamRecord{Repo: fixture.repo, Branch: "main", Commit: commit})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, fixture.root, "forks/widget/upstream.json", string(encoded)+"\n")
}

// commitRecordRepo writes an upstream record whose repo field is repo, and commits it.
func (fixture *syncFixture) commitRecordRepo(t *testing.T, repo, commit string) {
	t.Helper()
	encoded, err := json.Marshal(upstreamRecord{Repo: repo, Branch: "main", Commit: commit})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, fixture.root, "forks/widget/upstream.json", string(encoded)+"\n")
	gitTest(t, fixture.root, "add", ".")
	gitTest(t, fixture.root, "commit", "--allow-empty", "-m", "record repo")
}

// advanceUpstream commits a third upstream change and returns its commit.
func (fixture *syncFixture) advanceUpstream(t *testing.T) string {
	t.Helper()
	writeTestFile(t, fixture.upstream, "value.txt", "third\n")
	gitTest(t, fixture.upstream, "commit", "-am", "second upstream change")
	return strings.TrimSpace(string(gitOutput(t, fixture.upstream, "rev-parse", "HEAD")))
}

func (fixture *syncFixture) github(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/RigelBuild/nebula/pulls", func(w http.ResponseWriter, _ *http.Request) {
		if fixture.existingPR {
			reply(t, w, `[{"number":1,"title":"old sync title"}]`)
			return
		}
		reply(t, w, "[]")
	})
	mux.HandleFunc("POST /repos/RigelBuild/nebula/pulls", func(w http.ResponseWriter, r *http.Request) {
		fixture.createdPRs++
		var body struct {
			Title string `json:"title"`
			Head  string `json:"head"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode PR: %v", err)
		}
		fixture.title, fixture.branch = body.Title, body.Head
		w.WriteHeader(http.StatusCreated)
		reply(t, w, `{"number":1,"html_url":"https://example.test/pr/1"}`)
	})
	mux.HandleFunc("PATCH /repos/RigelBuild/nebula/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		fixture.updatedPRs++
		var body struct {
			Title string `json:"title"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode PR update: %v", err)
		}
		fixture.title = body.Title
		reply(t, w, `{"number":1}`)
	})
	mux.HandleFunc("POST /repos/RigelBuild/nebula/issues/1/labels", func(w http.ResponseWriter, r *http.Request) {
		var body []string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode labels: %v", err)
		}
		if len(body) > 0 {
			fixture.label = body[0]
		}
		reply(t, w, `[]`)
	})
	mux.HandleFunc("DELETE /repos/RigelBuild/nebula/issues/1/labels/conflict", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return httptest.NewServer(mux)
}

// reply writes a fake GitHub response. A failed write fails the test instead of being dropped.
func reply(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := w.Write([]byte(body)); err != nil {
		t.Errorf("write fake GitHub response: %v", err)
	}
}

func newGitHubClient(t *testing.T, endpoint string) *github.Client {
	t.Helper()
	base := endpoint + "/"
	client, err := github.NewClient(github.WithURLs(&base, nil))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func (fixture *syncFixture) recordedCommit(t *testing.T) string {
	t.Helper()
	var record upstreamRecord
	if err := json.Unmarshal(mustRead(t, filepath.Join(fixture.root, "forks/widget/upstream.json")), &record); err != nil {
		t.Fatal(err)
	}
	return record.Commit
}

// outboxBranch returns the commit a branch points to in the outbox repository.
func outboxBranch(t *testing.T, outbox, branch string) string {
	t.Helper()
	return strings.TrimSpace(string(gitOutput(t, outbox, "rev-parse", "refs/heads/"+branch)))
}
