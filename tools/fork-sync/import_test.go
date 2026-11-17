package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportRequiresExplicitBaseForUnrelatedHistory(t *testing.T) {
	root, forkRepo, upstreamRepo, base := createUnrelatedImportFixture(t)
	err := importFork(context.Background(), root, importOptions{
		Name:           "widget",
		Fork:           forkRepo,
		Ref:            "rigel-release",
		Upstream:       upstreamRepo,
		UpstreamBranch: "main",
	})
	if err == nil || !strings.Contains(err.Error(), "no merge-base") {
		t.Fatalf("import error = %v, want missing-merge-base error", err)
	}
	if _, err := os.Stat(filepath.Join(root, "forks/widget")); !os.IsNotExist(err) {
		t.Fatalf("fork tree exists after rejected import: %v", err)
	}
	if base == "" {
		t.Fatal("fixture upstream base is empty")
	}
}

func TestImportAcceptsExplicitBaseForUnrelatedHistory(t *testing.T) {
	root, forkRepo, upstreamRepo, base := createUnrelatedImportFixture(t)
	err := importFork(context.Background(), root, importOptions{
		Name:           "widget",
		Fork:           forkRepo,
		Ref:            "rigel-release",
		Upstream:       upstreamRepo,
		UpstreamBranch: "main",
		Base:           base,
	})
	if err != nil {
		t.Fatal(err)
	}
	var record upstreamRecord
	if err := json.Unmarshal(mustRead(t, filepath.Join(root, "forks/widget/upstream.json")), &record); err != nil {
		t.Fatal(err)
	}
	if record.Commit != base {
		t.Fatalf("recorded base = %s, want %s", record.Commit, base)
	}
	if got := string(mustRead(t, filepath.Join(root, "forks/widget/source.txt"))); got != "fork tree\n" {
		t.Fatalf("imported tree = %q", got)
	}
}

// TestImportRefusesOptionUpstream: an upstream that git would read as an option is refused before any fetch.
func TestImportRefusesOptionUpstream(t *testing.T) {
	root, forkRepo, _, _ := createUnrelatedImportFixture(t)
	marker := filepath.Join(t.TempDir(), "marker")
	err := importFork(context.Background(), root, importOptions{
		Name:           "widget",
		Fork:           forkRepo,
		Ref:            "rigel-release",
		Upstream:       "--upload-pack=touch " + marker,
		UpstreamBranch: "main",
	})
	requireRefusedRepo(t, err, marker)
}

// createUnrelatedImportFixture returns a nebula root, the fork and upstream as mapped https URLs, and the upstream base.
func createUnrelatedImportFixture(t *testing.T) (root, forkRepo, upstreamRepo, base string) {
	t.Helper()
	root = t.TempDir()
	gitTest(t, root, "init", "-b", "main")
	gitTest(t, root, "config", "user.name", "Test")
	gitTest(t, root, "config", "user.email", "test@example.com")
	writeTestFile(t, root, "README.md", "nebula\n")
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-m", "initial")

	upstream := filepath.Join(t.TempDir(), "upstream")
	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	gitTest(t, upstream, "init", "-b", "main")
	gitTest(t, upstream, "config", "user.name", "Test")
	gitTest(t, upstream, "config", "user.email", "test@example.com")
	writeTestFile(t, upstream, "README.md", "upstream\n")
	gitTest(t, upstream, "add", ".")
	gitTest(t, upstream, "commit", "-m", "upstream base")
	base = strings.TrimSpace(string(gitOutput(t, upstream, "rev-parse", "HEAD")))

	fork := filepath.Join(t.TempDir(), "fork.git")
	gitTest(t, t.TempDir(), "init", "--bare", "-b", "main", fork)
	forkWork := filepath.Join(t.TempDir(), "fork-work")
	gitTest(t, t.TempDir(), "clone", fork, forkWork)
	gitTest(t, forkWork, "config", "user.name", "Test")
	gitTest(t, forkWork, "config", "user.email", "test@example.com")
	writeTestFile(t, forkWork, "source.txt", "fork tree\n")
	gitTest(t, forkWork, "add", ".")
	gitTest(t, forkWork, "commit", "-m", "unrelated fork release")
	gitTest(t, forkWork, "branch", "-M", "rigel-release")
	gitTest(t, forkWork, "push", "origin", "rigel-release")
	mapRemotes(t, map[string]string{"fork": fork, "upstream": upstream})
	return root, "https://fixture.invalid/fork", "https://fixture.invalid/upstream", base
}

func TestImportWritesTreeRecordAndMoonProject(t *testing.T) {
	root := t.TempDir()
	gitTest(t, root, "init", "-b", "main")
	gitTest(t, root, "config", "user.name", "Test")
	gitTest(t, root, "config", "user.email", "test@example.com")
	writeTestFile(t, root, "README.md", "nebula\n")
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-m", "initial")
	upstream := filepath.Join(t.TempDir(), "upstream")
	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	gitTest(t, upstream, "init", "-b", "main")
	gitTest(t, upstream, "config", "user.name", "Test")
	gitTest(t, upstream, "config", "user.email", "test@example.com")
	writeTestFile(t, upstream, "README.md", "upstream\n")
	gitTest(t, upstream, "add", ".")
	gitTest(t, upstream, "commit", "-m", "upstream base")
	base := strings.TrimSpace(string(gitOutput(t, upstream, "rev-parse", "HEAD")))
	fork := filepath.Join(t.TempDir(), "fork.git")
	gitTest(t, t.TempDir(), "init", "--bare", "-b", "main", fork)
	forkWork := filepath.Join(t.TempDir(), "fork-work")
	gitTest(t, t.TempDir(), "clone", upstream, forkWork)
	gitTest(t, forkWork, "config", "user.name", "Test")
	gitTest(t, forkWork, "config", "user.email", "test@example.com")
	gitTest(t, forkWork, "remote", "set-url", "origin", fork)
	gitTest(t, forkWork, "checkout", "-b", "rigel-release")
	writeTestFile(t, forkWork, "source.txt", "fork tree\n")
	gitTest(t, forkWork, "add", ".")
	gitTest(t, forkWork, "commit", "-m", "fork release")
	gitTest(t, forkWork, "push", "origin", "rigel-release")
	mapRemotes(t, map[string]string{"fork": fork, "upstream": upstream})
	if err := importFork(context.Background(), root, importOptions{Name: "widget", Fork: "https://fixture.invalid/fork", Ref: "rigel-release", Upstream: "https://fixture.invalid/upstream", UpstreamBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, filepath.Join(root, "forks/widget/source.txt"))); got != "fork tree\n" {
		t.Fatalf("imported tree = %q", got)
	}
	if got := string(mustRead(t, filepath.Join(root, "forks/widget/upstream.json"))); !strings.Contains(got, base) {
		t.Fatalf("upstream record does not contain merge-base %s: %s", base, got)
	}
	// The stub must be a valid moon project file: an object-form project, never a string.
	if got := string(mustRead(t, filepath.Join(root, "forks/widget/moon.yml"))); strings.Contains(got, "project: widget") || !strings.Contains(got, "project:\n  name: \"widget\"\n") {
		t.Fatalf("moon project stub is not object-form: %q", got)
	}
	if got := strings.TrimSpace(string(gitOutput(t, root, "log", "-1", "--pretty=%s"))); !strings.Contains(got, short(base)) {
		t.Fatalf("import commit subject = %q", got)
	}
}
