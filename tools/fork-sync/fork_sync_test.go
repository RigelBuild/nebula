package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-github/v93/github"
)

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

func TestExportAppliesToOutbox(t *testing.T) {
	fixture := newSyncFixture(t, false)
	outbox := filepath.Join(t.TempDir(), "outbox.git")
	gitTest(t, t.TempDir(), "init", "--bare", "-b", "main", outbox)
	gitTest(t, fixture.upstream, "push", outbox, fixture.oldCommit+":refs/heads/main")
	worktree := filepath.Join(t.TempDir(), "outbox-work")
	gitTest(t, t.TempDir(), "clone", outbox, worktree)
	gitTest(t, worktree, "config", "user.name", "Test")
	gitTest(t, worktree, "config", "user.email", "test@example.com")
	writeTestFile(t, fixture.root, "forks/widget/value.txt", "exported\n")
	gitTest(t, fixture.root, "add", ".")
	gitTest(t, fixture.root, "commit", "-m", "export change")
	start := strings.TrimSpace(string(gitOutput(t, fixture.root, "rev-parse", "HEAD^")))
	end := strings.TrimSpace(string(gitOutput(t, fixture.root, "rev-parse", "HEAD")))
	if err := exportFork(context.Background(), fixture.root, exportOptions{Name: "widget", Range: start + ".." + end, Topic: "test", Outbox: outbox}); err != nil {
		t.Fatal(err)
	}
	gitTest(t, worktree, "fetch", "origin", "refs/heads/upstream/test")
	if got := string(gitOutput(t, worktree, "show", "FETCH_HEAD:value.txt")); got != "exported\n" {
		t.Fatalf("exported tree content = %q", got)
	}
}

func TestImportRequiresExplicitBaseForUnrelatedHistory(t *testing.T) {
	root, fork, upstream, base := createUnrelatedImportFixture(t)
	err := importFork(context.Background(), root, importOptions{
		Name:           "widget",
		Fork:           fork,
		Ref:            "rigel-release",
		Upstream:       upstream,
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
	root, fork, upstream, base := createUnrelatedImportFixture(t)
	err := importFork(context.Background(), root, importOptions{
		Name:           "widget",
		Fork:           fork,
		Ref:            "rigel-release",
		Upstream:       upstream,
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

func createUnrelatedImportFixture(t *testing.T) (root, fork, upstream, base string) {
	t.Helper()
	root = t.TempDir()
	gitTest(t, root, "init", "-b", "main")
	gitTest(t, root, "config", "user.name", "Test")
	gitTest(t, root, "config", "user.email", "test@example.com")
	writeTestFile(t, root, "README.md", "nebula\n")
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-m", "initial")

	upstream = filepath.Join(t.TempDir(), "upstream")
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

	fork = filepath.Join(t.TempDir(), "fork.git")
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
	return root, fork, upstream, base
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
	if err := importFork(context.Background(), root, importOptions{Name: "widget", Fork: fork, Ref: "rigel-release", Upstream: upstream, UpstreamBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, filepath.Join(root, "forks/widget/source.txt"))); got != "fork tree\n" {
		t.Fatalf("imported tree = %q", got)
	}
	if got := string(mustRead(t, filepath.Join(root, "forks/widget/upstream.json"))); !strings.Contains(got, base) {
		t.Fatalf("upstream record does not contain merge-base %s: %s", base, got)
	}
	if _, err := os.Stat(filepath.Join(root, "forks/widget/moon.yml")); err != nil {
		t.Fatalf("moon project stub missing: %v", err)
	}
	if got := strings.TrimSpace(string(gitOutput(t, root, "log", "-1", "--pretty=%s"))); !strings.Contains(got, short(base)) {
		t.Fatalf("import commit subject = %q", got)
	}
}

func createTestRepository(t *testing.T, bare, worktree string) {
	t.Helper()
	gitTest(t, t.TempDir(), "init", "--bare", "-b", "main", bare)
	gitTest(t, t.TempDir(), "clone", bare, worktree)
	gitTest(t, worktree, "config", "user.name", "Test")
	gitTest(t, worktree, "config", "user.email", "test@example.com")
	writeTestFile(t, worktree, "README.md", "base tree\n")
	gitTest(t, worktree, "add", ".")
	gitTest(t, worktree, "commit", "-m", "upstream base")
	gitTest(t, worktree, "push", "origin", "main")
}

type syncFixture struct {
	root, upstream, newCommit, oldCommit string
	config                               forkConfig
	title, branch, label                 string
	existingPR                           bool
	createdPRs, updatedPRs               int
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
	fixture := &syncFixture{root: root, upstream: upstream, oldCommit: old, newCommit: newCommit, config: forkConfig{Name: "widget", Root: root, BaseBranch: "main", Repository: "RigelBuild/nebula"}}
	fixture.writeRecord(t, old)
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-m", "initial")
	outbox := filepath.Join(t.TempDir(), "outbox.git")
	gitTest(t, t.TempDir(), "init", "--bare", "-b", "main", outbox)
	gitTest(t, root, "remote", "add", "origin", outbox)
	gitTest(t, root, "push", "origin", "main")
	fixture.config.Outbox = outbox
	return fixture
}

func (fixture *syncFixture) writeRecord(t *testing.T, commit string) {
	t.Helper()
	encoded, err := json.Marshal(upstreamRecord{Repo: fixture.upstream, Branch: "main", Commit: commit})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, fixture.root, "forks/widget/upstream.json", string(encoded)+"\n")
}

func (fixture *syncFixture) github(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/RigelBuild/nebula/pulls", func(w http.ResponseWriter, _ *http.Request) {
		if fixture.existingPR {
			_, _ = w.Write([]byte(`[{"number":1,"title":"old sync title"}]`))
			return
		}
		_, _ = w.Write([]byte("[]"))
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
		_, _ = w.Write([]byte(`{"number":1,"html_url":"https://example.test/pr/1"}`))
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
		_, _ = w.Write([]byte(`{"number":1}`))
	})
	mux.HandleFunc("POST /repos/RigelBuild/nebula/issues/1/labels", func(w http.ResponseWriter, r *http.Request) {
		var body []string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode labels: %v", err)
		}
		if len(body) > 0 {
			fixture.label = body[0]
		}
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("DELETE /repos/RigelBuild/nebula/issues/1/labels/conflict", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return httptest.NewServer(mux)
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

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeTestFile(t *testing.T, root, path, content string) {
	t.Helper()
	absolute := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolute, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
func gitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
func gitOutput(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return output
}
