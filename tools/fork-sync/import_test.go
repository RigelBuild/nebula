package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
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

// TestImportRefusesOptionFork: a fork that git would read as an option is refused before any fetch.
func TestImportRefusesOptionFork(t *testing.T) {
	root, _, upstreamRepo, _ := createUnrelatedImportFixture(t)
	marker := filepath.Join(t.TempDir(), "marker")
	err := importFork(context.Background(), root, importOptions{
		Name:           "widget",
		Fork:           "--upload-pack=touch " + marker,
		Ref:            "rigel-release",
		Upstream:       upstreamRepo,
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

// TestImportKeepsForkBlobsByteIdentical: the import must copy each fork blob as stored. An archive-based
// import applies the fork's .gitattributes: eol=crlf rewrites line endings, export-ignore drops a path,
// and export-subst expands $Format:...$.
func TestImportKeepsForkBlobsByteIdentical(t *testing.T) {
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
	fork := filepath.Join(t.TempDir(), "fork.git")
	gitTest(t, t.TempDir(), "init", "--bare", "-b", "main", fork)
	forkWork := filepath.Join(t.TempDir(), "fork-work")
	gitTest(t, t.TempDir(), "clone", upstream, forkWork)
	gitTest(t, forkWork, "config", "user.name", "Test")
	gitTest(t, forkWork, "config", "user.email", "test@example.com")
	gitTest(t, forkWork, "remote", "set-url", "origin", fork)
	gitTest(t, forkWork, "checkout", "-b", "rigel-release")
	writeTestFile(t, forkWork, ".gitattributes", "*.ps1 text eol=crlf\nignored.txt export-ignore\nsubst.txt export-subst\n")
	writeTestFile(t, forkWork, "a.ps1", "Write-Output 'a'\n")
	writeTestFile(t, forkWork, "ignored.txt", "kept\n")
	writeTestFile(t, forkWork, "subst.txt", "$Format:%H$\n")
	gitTest(t, forkWork, "add", ".")
	gitTest(t, forkWork, "commit", "-m", "fork release")
	gitTest(t, forkWork, "push", "origin", "rigel-release")
	forkCommit := strings.TrimSpace(string(gitOutput(t, forkWork, "rev-parse", "HEAD")))
	mapRemotes(t, map[string]string{"fork": fork, "upstream": upstream})
	if err := importFork(context.Background(), root, importOptions{Name: "widget", Fork: "https://fixture.invalid/fork", Ref: "rigel-release", Upstream: "https://fixture.invalid/upstream", UpstreamBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "forks/widget/ignored.txt")); err != nil || string(got) != "kept\n" {
		t.Errorf("forks/widget/ignored.txt = %q, %v; want %q", got, err, "kept\n")
	}
	// git add renormalizes eol, so the index blob hides CRLF. The worktree bytes must match the fork's.
	if got, err := os.ReadFile(filepath.Join(root, "forks/widget/a.ps1")); err != nil || string(got) != "Write-Output 'a'\n" {
		t.Errorf("forks/widget/a.ps1 = %q, %v; want the fork's LF bytes", got, err)
	}
	forkBlobs := blobsByPath(t, fork, forkCommit, "")
	importBlobs := blobsByPath(t, root, "HEAD", "forks/widget")
	for path, want := range forkBlobs {
		if got := importBlobs[path]; got != want {
			t.Errorf("forks/widget/%s blob = %q, want fork blob %s", path, got, want)
		}
	}
}

// TestImportCommitBlobsMatchForkWithoutAttributeFilters: git add must not run the fork's clean filters. A fork blob
// stored with CRLF under text, or with $Id: ... $ under ident, must reach the import commit unchanged.
func TestImportCommitBlobsMatchForkWithoutAttributeFilters(t *testing.T) {
	root, forkRepo, upstreamRepo, forkCommit, forkDir := createFilterFixture(t, func(fork string) {
		writeRawBlob(t, fork, "crlf.txt", "a\r\nb\r\n")
		writeRawBlob(t, fork, "id.c", "$Id: deadbeef $\n")
		writeRawBlob(t, fork, ".gitattributes", "*.txt text\nid.c ident\n")
	})
	if err := importFork(context.Background(), root, importOptions{Name: "widget", Fork: forkRepo, Ref: "rigel-release", Upstream: upstreamRepo, UpstreamBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	forkBlobs := blobsByPath(t, forkDir, forkCommit, "")
	commitBlobs := blobsByPath(t, root, "HEAD", "forks/widget")
	for _, path := range []string{"crlf.txt", "id.c", ".gitattributes"} {
		if got := commitBlobs[path]; got != forkBlobs[path] {
			t.Errorf("commit blob for %s = %s, want fork blob %s", path, got, forkBlobs[path])
		}
	}
}

// TestImportPreservesModesSymlinksAndGitlinks: 100755 keeps its exec bit, a 120000 link keeps its target (even one
// outside the tree), and a 160000 gitlink is staged as a gitlink with no object in the fork store.
func TestImportPreservesModesSymlinksAndGitlinks(t *testing.T) {
	root, forkRepo, upstreamRepo, forkCommit, forkDir := createFilterFixture(t, func(fork string) {
		gitTest(t, fork, "update-index", "--add", "--cacheinfo", "100755,"+hashRaw(t, fork, "#!/bin/sh\necho hi\n")+",run.sh")
		gitTest(t, fork, "update-index", "--add", "--cacheinfo", "120000,"+hashRaw(t, fork, "../../../../outside")+",link")
		gitTest(t, fork, "update-index", "--add", "--cacheinfo", "160000,0123456789abcdef0123456789abcdef01234567,sub")
	})
	if err := importFork(context.Background(), root, importOptions{Name: "widget", Fork: forkRepo, Ref: "rigel-release", Upstream: upstreamRepo, UpstreamBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(root, "forks/widget/run.sh")); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("run.sh mode = %v, %v; want executable", info, err)
	}
	if got, err := os.Readlink(filepath.Join(root, "forks/widget/link")); err != nil || got != "../../../../outside" {
		t.Errorf("link target = %q, %v; want ../../../../outside", got, err)
	}
	want := blobsByPath(t, forkDir, forkCommit, "")
	got := blobsByPath(t, root, "HEAD", "forks/widget")
	for _, path := range []string{"run.sh", "link"} {
		if got[path] != want[path] {
			t.Errorf("commit blob for %s = %s, want fork blob %s", path, got[path], want[path])
		}
	}
	if mode := commitMode(t, root, "forks/widget/sub"); mode != "160000" {
		t.Errorf("forks/widget/sub mode = %q, want 160000 gitlink", mode)
	}
	if mode := commitMode(t, root, "forks/widget/run.sh"); mode != "100755" {
		t.Errorf("forks/widget/run.sh committed mode = %q, want 100755", mode)
	}
}

// TestImportAvoidsMaintenanceChildProcess: a detached git maintenance run can race object reads. The fixture
// environment disables it, so no traced git invocation spawns a maintenance child.
func TestImportAvoidsMaintenanceChildProcess(t *testing.T) {
	root, forkRepo, upstreamRepo, _, _ := createFilterFixture(t, func(string) {})
	trace := filepath.Join(t.TempDir(), "trace.json")
	t.Setenv("GIT_TRACE2_EVENT", trace)
	if err := importFork(context.Background(), root, importOptions{Name: "widget", Fork: forkRepo, Ref: "rigel-release", Upstream: upstreamRepo, UpstreamBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mustRead(t, trace)), `"maintenance"`) {
		t.Fatal("a git maintenance child ran during import")
	}
}

// TestImportReadsBlobsInOneCatFileStream: blob reads must share one `git cat-file --batch` process, not one
// process per blob. The fork has several blobs; a per-blob read starts one cat-file child for each.
func TestImportReadsBlobsInOneCatFileStream(t *testing.T) {
	root, forkRepo, upstreamRepo, _, _ := createFilterFixture(t, func(fork string) {
		for _, name := range []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"} {
			writeRawBlob(t, fork, name, name+"\n")
		}
	})
	trace := filepath.Join(t.TempDir(), "trace.json")
	t.Setenv("GIT_TRACE2_EVENT", trace)
	if err := importFork(context.Background(), root, importOptions{Name: "widget", Fork: forkRepo, Ref: "rigel-release", Upstream: upstreamRepo, UpstreamBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(mustRead(t, trace)), `"argv":["git","cat-file","--batch"]`); n != 1 {
		t.Fatalf("cat-file --batch ran %d times; want one blob stream", n)
	}
}

// createFilterFixture builds a nebula root and a fork whose tree is written by edit, then returns both URLs and the fork commit.
func createFilterFixture(t *testing.T, edit func(fork string)) (root, forkRepo, upstreamRepo, forkCommit, forkDir string) {
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
	fork := filepath.Join(t.TempDir(), "fork.git")
	gitTest(t, t.TempDir(), "init", "--bare", "-b", "main", fork)
	forkWork := filepath.Join(t.TempDir(), "fork-work")
	gitTest(t, t.TempDir(), "clone", upstream, forkWork)
	gitTest(t, forkWork, "config", "user.name", "Test")
	gitTest(t, forkWork, "config", "user.email", "test@example.com")
	gitTest(t, forkWork, "remote", "set-url", "origin", fork)
	gitTest(t, forkWork, "checkout", "-b", "rigel-release")
	edit(forkWork)
	gitTest(t, forkWork, "commit", "--allow-empty", "-m", "fork release")
	gitTest(t, forkWork, "push", "origin", "rigel-release")
	forkCommit = strings.TrimSpace(string(gitOutput(t, forkWork, "rev-parse", "HEAD")))
	mapRemotes(t, map[string]string{"fork": fork, "upstream": upstream})
	return root, "https://fixture.invalid/fork", "https://fixture.invalid/upstream", forkCommit, forkWork
}

// writeRawBlob stores content as a blob with no filters and adds it to the fork index at path.
func writeRawBlob(t *testing.T, fork, path, content string) {
	t.Helper()
	gitTest(t, fork, "update-index", "--add", "--cacheinfo", "100644,"+hashRaw(t, fork, content)+","+path)
}

// hashRaw writes content to the object store with no filters and returns its object ID.
func hashRaw(t *testing.T, dir, content string) string {
	t.Helper()
	cmd := exec.Command("git", "hash-object", "-w", "--no-filters", "--stdin")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(content)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("hash-object: %v", err)
	}
	return strings.TrimSpace(string(output))
}

// commitMode returns the mode of path in the HEAD commit of root.
func commitMode(t *testing.T, root, path string) string {
	t.Helper()
	fields := strings.Fields(string(gitOutput(t, root, "ls-tree", "HEAD", "--", path)))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// blobsByPath maps each blob under prefix in rev to its object ID. Keys are relative to prefix; an empty prefix means the whole tree.
func blobsByPath(t *testing.T, dir, rev, prefix string) map[string]string {
	t.Helper()
	args := []string{"ls-tree", "-r", rev}
	if prefix != "" {
		args = append(args, "--", prefix)
	}
	blobs := make(map[string]string)
	for line := range strings.SplitSeq(string(gitOutput(t, dir, args...)), "\n") {
		if line == "" {
			continue
		}
		meta, path, found := strings.Cut(line, "\t")
		if !found {
			t.Fatalf("ls-tree line %q has no path", line)
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 || fields[1] != "blob" {
			continue
		}
		blobs[strings.TrimPrefix(path, prefix+"/")] = fields[2]
	}
	return blobs
}

func TestForkRepoURL(t *testing.T) {
	tests := []struct {
		name string
		fork string
		want string
	}{
		{
			name: "RigelBuild shorthand keeps the owner",
			fork: "RigelBuild/virtual",
			want: "https://github.com/RigelBuild/virtual.git",
		},
		{
			name: "https URL passes through",
			fork: "https://github.com/RigelBuild/virtual.git",
			want: "https://github.com/RigelBuild/virtual.git",
		},
		{
			name: "ssh URL passes through",
			fork: "ssh://git@github.com/RigelBuild/virtual.git",
			want: "ssh://git@github.com/RigelBuild/virtual.git",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := forkRepoURL(tt.fork); got != tt.want {
				t.Fatalf("forkRepoURL(%q) = %q, want %q", tt.fork, got, tt.want)
			}
		})
	}
}
