package main

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

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
	start, end := headRange(t, fixture.root)
	var output strings.Builder
	if err := exportFork(context.Background(), fixture.root, exportOptions{Name: "widget", Range: start + ".." + end, Topic: "test", Outbox: outbox}, &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	sha := strings.TrimSpace(string(gitOutput(t, outbox, "rev-parse", "refs/heads/upstream/test")))
	if want := []string{"pushed " + short(sha) + " to refs/heads/upstream/test", "compare: outbox " + outbox + " branch upstream/test (base " + short(fixture.oldCommit) + ")"}; !slices.Equal(lines, want) {
		t.Fatalf("export output = %q, want %q", lines, want)
	}
	gitTest(t, worktree, "fetch", "origin", "refs/heads/upstream/test")
	if got := string(gitOutput(t, worktree, "show", "FETCH_HEAD:value.txt")); got != "exported\n" {
		t.Fatalf("exported tree content = %q", got)
	}
}

// TestExportExcludesNebulaOwnedFiles: moon.yml, upstream.json, and component.cue belong to nebula, so an export must not carry them.
func TestExportExcludesNebulaOwnedFiles(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(t *testing.T, fixture *syncFixture)
	}{
		{name: "moon.yml", edit: func(t *testing.T, fixture *syncFixture) {
			writeTestFile(t, fixture.root, "forks/widget/moon.yml", "project:\n  name: \"widget\"\n")
		}},
		{name: "upstream.json", edit: func(t *testing.T, fixture *syncFixture) {
			encoded, err := json.MarshalIndent(upstreamRecord{Repo: fixture.repo, Branch: "main", Commit: fixture.oldCommit}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, fixture.root, "forks/widget/upstream.json", string(encoded)+"\n")
		}},
		{name: "component.cue", edit: func(t *testing.T, fixture *syncFixture) {
			writeTestFile(t, fixture.root, "forks/widget/component.cue", "id: \"widget\"\n")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSyncFixture(t, false)
			writeTestFile(t, fixture.root, "forks/widget/value.txt", "exported\n")
			test.edit(t, fixture)
			gitTest(t, fixture.root, "add", ".")
			gitTest(t, fixture.root, "commit", "-m", "export change")
			start, end := headRange(t, fixture.root)
			if err := exportFork(context.Background(), fixture.root, exportOptions{Name: "widget", Range: start + ".." + end, Topic: "test", Outbox: fixture.outbox}, io.Discard); err != nil {
				t.Fatal(err)
			}
			files := strings.Fields(string(gitOutput(t, fixture.outbox, "ls-tree", "-r", "--name-only", "refs/heads/upstream/test")))
			if want := []string{"value.txt"}; !slices.Equal(files, want) {
				t.Fatalf("outbox files = %v, want %v", files, want)
			}
		})
	}
}

// TestExportRefusesOptionRecordRepo: a record repo that git would read as an option is refused before any fetch.
func TestExportRefusesOptionRecordRepo(t *testing.T) {
	fixture := newSyncFixture(t, false)
	marker := filepath.Join(t.TempDir(), "marker")
	fixture.commitRecordRepo(t, "--upload-pack=touch "+marker, fixture.oldCommit)
	writeTestFile(t, fixture.root, "forks/widget/value.txt", "exported\n")
	gitTest(t, fixture.root, "add", ".")
	gitTest(t, fixture.root, "commit", "-m", "export change")
	start, end := headRange(t, fixture.root)
	err := exportFork(context.Background(), fixture.root, exportOptions{Name: "widget", Range: start + ".." + end, Topic: "test", Outbox: fixture.outbox}, io.Discard)
	requireRefusedRepo(t, err, marker)
}

// TestExportRefusesOptionOutbox: an outbox that git would read as an option is refused before any push.
func TestExportRefusesOptionOutbox(t *testing.T) {
	fixture := newSyncFixture(t, false)
	err := exportFork(context.Background(), fixture.root, exportOptions{Name: "widget", Range: "HEAD^..HEAD", Topic: "test", Outbox: "--fork-sync-test-option"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "refused outbox") {
		t.Fatalf("error = %v, want a refused outbox", err)
	}
}
