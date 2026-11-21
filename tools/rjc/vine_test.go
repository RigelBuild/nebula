package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("RJC_TEST_HELPER") == "1" {
		os.Exit(toolHelper())
	}
	os.Exit(runTestsOnTmpfs(m))
}

// minTmpfsFree is the free space /dev/shm must have before fixtures use it.
const minTmpfsFree = 512 << 20

// staleTmpfsParentAge is how old an rjc-test-* parent must be before a later
// run reaps it. It covers parents left by a run that was killed.
const staleTmpfsParentAge = time.Hour

// runTestsOnTmpfs runs the package tests with TMPDIR set to a per-run parent
// under /dev/shm when that tmpfs has room. The parent is removed after the
// run. A caller-set TMPDIR is respected. Hosts without enough room, or where
// the tmpfs cannot be used, run on the default temp dir.
func runTestsOnTmpfs(m *testing.M) int {
	if os.Getenv("TMPDIR") != "" {
		return m.Run()
	}
	parent := makeTmpfsParent()
	if parent == "" {
		return m.Run()
	}
	defer func() {
		if err := os.RemoveAll(parent); err != nil {
			slog.Error("remove tmpfs test parent", "dir", parent, "err", err)
		}
	}()
	if err := os.Setenv("TMPDIR", parent); err != nil {
		slog.Error("set TMPDIR", "err", err)
		return 1
	}
	return m.Run()
}

// makeTmpfsParent creates a per-run parent under /dev/shm. It returns "" when
// /dev/shm is missing, has less than minTmpfsFree free, or cannot be used. A
// failure is logged as a warning and the caller keeps the default temp dir.
func makeTmpfsParent() string {
	const tmpfsDir = "/dev/shm"
	info, err := os.Stat(tmpfsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		slog.Warn("stat tmpfs, using default TMPDIR", "dir", tmpfsDir, "err", err)
		return ""
	}
	if !info.IsDir() {
		return ""
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(tmpfsDir, &st); err != nil {
		slog.Warn("statfs tmpfs, using default TMPDIR", "dir", tmpfsDir, "err", err)
		return ""
	}
	if st.Bavail*uint64(st.Bsize) < minTmpfsFree {
		return ""
	}
	reapStaleTmpfsParents(tmpfsDir)
	parent, err := os.MkdirTemp(tmpfsDir, "rjc-test-")
	if err != nil {
		slog.Warn("mkdir tmpfs test parent, using default TMPDIR", "dir", tmpfsDir, "err", err)
		return ""
	}
	return parent
}

// reapStaleTmpfsParents removes rjc-test-* parents older than
// staleTmpfsParentAge. A failure to reap one is logged and does not stop the run.
func reapStaleTmpfsParents(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		slog.Warn("read tmpfs dir for stale parents", "dir", dir, "err", err)
		return
	}
	cutoff := time.Now().Add(-staleTmpfsParentAge)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "rjc-test-") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(path); err != nil {
			slog.Warn("reap stale tmpfs test parent", "dir", path, "err", err)
		}
	}
}

func toolHelper() int {
	if filepath.Base(os.Args[0]) == "jj-hp" {
		return hookToolHelper()
	}
	if filepath.Base(os.Args[0]) == "moon" {
		return moonToolHelper()
	}
	if filepath.Base(os.Args[0]) == "hk" {
		return hkToolHelper()
	}
	if filepath.Base(os.Args[0]) == "gh" {
		return ghAPIHelper()
	}
	if os.Getenv("GH_TOKEN") != "" || os.Getenv("GITHUB_TOKEN") != "" {
		return 93
	}
	if filepath.Base(os.Args[0]) == "jj" {
		return proxyFixtureJJ()
	}
	if filepath.Base(os.Args[0]) == "rjc-token" {
		if !slices.Equal(os.Args[1:], []string{"github.com", "one argument with spaces", `literal"quote`}) {
			return 95
		}
		if _, err := fmt.Fprintln(os.Stdout, "fixture-token"); err != nil {
			return 94
		}
		return 0
	}
	if code := checkVineHelper(); code != 0 {
		return code
	}
	if os.Getenv("RJC_TEST_VINE_SUCCESS") == "1" {
		return 0
	}
	if _, err := fmt.Fprintln(os.Stdout, "stub push stdout"); err != nil {
		return 94
	}
	if _, err := fmt.Fprintln(os.Stderr, "stub push stderr"); err != nil {
		return 94
	}
	return 37
}

func proxyFixtureJJ() int {
	for i, arg := range os.Args[1:] {
		if arg == "fetch" && os.Getenv("RJC_TEST_FAIL_FETCH") == "1" {
			return 37
		}
		if arg == "bookmark" && i+2 < len(os.Args) && os.Args[i+2] == "create" && os.Getenv("RJC_TEST_FAIL_BOOKMARK") == "1" {
			return 37
		}
		if arg == "new" && i+2 < len(os.Args) && os.Args[i+2] == os.Getenv("RJC_TEST_FAIL_NEW_AT") {
			return 37
		}
		if bookmark := os.Getenv("RJC_TEST_EXACT_BOOKMARK"); bookmark != "" && arg == "bookmarks("+quoteRev(bookmark)+")" {
			return 38
		}
	}
	cmd := exec.Command(os.Getenv("RJC_TEST_REAL_JJ"), os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		return 96
	}
	return 0
}

func checkVineHelper() int {
	r := &repository{}
	if code := checkVineTarget(r); code != 0 {
		return code
	}
	var expected map[string]string
	if raw := os.Getenv("RJC_TEST_VINE_CONFIG"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &expected); err != nil {
			return 96
		}
	}
	for key, want := range expected {
		got, err := r.jj(context.Background(), "config", "get", key)
		if err != nil || got != want {
			return 96
		}
	}
	return 0
}

func checkVineTarget(r *repository) int {
	foreign := os.Getenv("RJC_TEST_FOREIGN")
	if foreign == "" {
		return 0
	}
	selected, err := r.one(context.Background(), os.Args[2])
	if err != nil {
		return 96
	}
	if path := os.Getenv("RJC_TEST_RESOLVED"); path != "" {
		if err := os.WriteFile(path, []byte(selected), 0o600); err != nil {
			return 96
		}
	}
	foreignTip, err := r.one(context.Background(), quoteRev(foreign))
	if err != nil {
		return 96
	}
	if selected == foreignTip {
		if err := os.WriteFile(os.Getenv("RJC_TEST_FOREIGN_WRITE"), []byte("foreign push and PR write"), 0o600); err != nil {
			return 96
		}
	}
	return 0
}

func expectVineConfig(t *testing.T, values map[string]string) {
	t.Helper()
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RJC_TEST_VINE_CONFIG", string(data))
}

func TestVineStreamingAndTokenIsolation(t *testing.T) {
	f := fixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): stream failed push output", "")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, name := range []string{"jj-vine", "rjc-token"} {
		if err := os.Symlink(executable, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RJC_TEST_HELPER", "1")
	t.Setenv("GH_TOKEN", "inherited-fixture-token")
	t.Setenv("GITHUB_TOKEN", "inherited-fixture-token")
	bodyPath, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).candidate(bookmark, "authored body\n")
	if err != nil {
		t.Fatal(err)
	}
	line, err := f.repo.line(f.ctx, bookmark, lineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before := jjTest(t, f.repo, "config", "list", "--workspace", "jj-vine")
	expectVineConfig(t, map[string]string{
		"jj-vine.description.singleRevision":    "file(" + bodyPath + ")",
		"jj-vine.description.multipleRevisions": "file(" + bodyPath + ")",
		"jj-vine.openAsDraft":                   "false",
	})
	var log bytes.Buffer
	if err := (vine{repo: f.repo, line: line}).Submit(f.ctx, bookmark, bodyPath, false, false, &log); err == nil {
		t.Fatal("stub push failure was swallowed")
	}
	if !strings.Contains(log.String(), "stub push stdout") || !strings.Contains(log.String(), "stub push stderr") {
		t.Fatalf("streamed log = %q", log.String())
	}
	if after := jjTest(t, f.repo, "config", "list", "--workspace", "jj-vine"); after != before {
		t.Fatalf("engine failure leaked config: before=%q after=%q", before, after)
	}
	jjTest(t, f.repo, "config", "set", "--workspace", "jj-vine.github.tokenCommand", `["`+filepath.Join(bin, "rjc-token")+`", "{host}", "one argument with spaces", 'literal"quote']`)
	f.repo.log = &log
	forge, err := f.repo.forge(f.ctx)
	if err != nil || forge.token != "fixture-token" {
		t.Fatalf("token command failed: %v", err)
	}
	if strings.Contains(log.String(), "fixture-token") {
		t.Fatal("token command leaked its output into the log")
	}
}

func TestVineConfiguresForeignParentBoundary(t *testing.T) {
	f := fixtureRepo(t)
	parent := f.new(t, "EX-1", "feat(rjc): preserve a foreign boundary", "")
	foreign := strings.Replace(parent, "test-lane/", "other-lane/", 1)
	jjTest(t, f.repo, "bookmark", "rename", parent, foreign)
	jjTest(t, f.repo, "git", "push", "--bookmark", foreign)
	bookmark := f.new(t, "EX-2", "feat(rjc): bound the actual engine selection", foreign)
	line, err := f.repo.line(f.ctx, bookmark, lineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(executable, filepath.Join(bin, "jj-vine")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RJC_TEST_HELPER", "1")
	jjTest(t, f.repo, "config", "set", "--workspace", "jj-vine.defaultBaseBranch", "previous-parent")
	jjTest(t, f.repo, "config", "set", "--workspace", "jj-vine.description.sync", "true")
	before := jjTest(t, f.repo, "config", "list", "--workspace", "jj-vine")
	// A foreign parent is the validated base, not the trunk.
	expectVineConfig(t, map[string]string{"jj-vine.defaultBaseBranch": foreign, "jj-vine.remoteName": "origin"})
	var log bytes.Buffer
	if err := (vine{repo: f.repo, line: line}).Submit(f.ctx, bookmark, f.bodyFile, false, false, &log); err == nil {
		t.Fatal("stub engine failure was swallowed")
	}
	if !strings.Contains(log.String(), "stub push stdout") {
		t.Fatal("engine was not invoked")
	}
	if after := jjTest(t, f.repo, "config", "list", "--workspace", "jj-vine"); after != before {
		t.Fatalf("foreign parent config leaked: before=%q after=%q", before, after)
	}
	t.Setenv("RJC_TEST_VINE_SUCCESS", "1")
	if err := (vine{repo: f.repo, line: line}).Submit(f.ctx, bookmark, f.bodyFile, false, false, &log); err != nil {
		t.Fatal(err)
	}
	if after := jjTest(t, f.repo, "config", "list", "--workspace", "jj-vine"); after != before {
		t.Fatalf("engine success leaked config: before=%q after=%q", before, after)
	}
}

func TestVineLiteralTargetIgnoresForeignAlias(t *testing.T) {
	f := fixtureRepo(t)
	foreign := f.new(t, "EX-1", "feat(rjc): foreign alias target", "")
	renamed := strings.Replace(foreign, "test-lane/", "other-lane/", 1)
	jjTest(t, f.repo, "bookmark", "rename", foreign, renamed)
	bookmark := f.new(t, "EX-2", "feat(rjc): select the literal owned bookmark", "")
	f.commit(t, "alias.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	jjTest(t, f.repo, "config", "set", "--workspace", `revset-aliases."`+bookmark+`"`, quoteRev(renamed))
	expected := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	control := jjTest(t, f.repo, "log", "--no-graph", "-r", bookmark, "-T", "commit_id")
	if control == expected {
		t.Fatal("bare alias control did not resolve differently")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(executable, filepath.Join(bin, "jj-vine")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RJC_TEST_HELPER", "1")
	t.Setenv("RJC_TEST_FOREIGN", renamed)
	marker := filepath.Join(t.TempDir(), "foreign-write")
	t.Setenv("RJC_TEST_FOREIGN_WRITE", marker)
	resolved := filepath.Join(t.TempDir(), "resolved-target")
	t.Setenv("RJC_TEST_RESOLVED", resolved)
	err = f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): select the literal owned bookmark", bodyFile: f.bodyFile}, vine{repo: f.repo}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("stub failure was swallowed")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("foreign push or PR write: %v", err)
	}
	actual, err := os.ReadFile(resolved)
	if err != nil || string(actual) != expected {
		t.Fatalf("engine did not resolve the owned target: got=%s want=%s err=%v", actual, expected, err)
	}
	if len(f.prs) != 0 || len(f.pushes) != 0 {
		t.Fatal("alias caused a forge or push write")
	}
}
