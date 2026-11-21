package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseLsofCwds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		text    string
		want    []string
		wantErr bool
	}{
		{name: "cwd records are listed", text: "p101\nn/a/b\np202\nn/c\n", want: []string{"/a/b", "/c"}},
		{name: "empty output is refused", text: "", wantErr: true},
		{name: "pid lines without a cwd are refused", text: "p101\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseLsofCwds(tc.text)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %t", err, tc.wantErr)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("cwds = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReportStateKeysAndPrune(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first := kept{root: root, line: "x", key: reportKeyOf(root, []string{"abc"})}
	if got := newKept([]kept{first}, reportState{}); len(got) != 1 {
		t.Fatalf("new workspace not reported: %+v", got)
	}
	st := reportState{Reported: []string{reportKey(first)}}
	if got := newKept([]kept{first}, st); len(got) != 0 {
		t.Fatalf("already reported workspace reported again: %+v", got)
	}
	changed := kept{root: root, line: "x", key: reportKeyOf(root, []string{"abc", "def"})}
	if got := newKept([]kept{changed}, st); len(got) != 1 {
		t.Fatalf("changed unpushed set not reported: %+v", got)
	}
	gone := filepath.Join(t.TempDir(), "gone")
	stale := reportState{Reported: []string{reportKeyOf(root, []string{"abc"}), reportKeyOf(gone, nil)}}
	pruned := pruneReported(stale)
	if len(pruned.Reported) != 1 || keyRoot(pruned.Reported[0]) != root {
		t.Fatalf("prune kept %q, want only the existing root", pruned.Reported)
	}
}

func TestReportNotRecordedWhenSendFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config{apply: true, cotalBin: filepath.Join(dir, "no-such-cotal"), to: "nemo"}
	k := kept{root: dir, line: "w", key: reportKeyOf(dir, []string{"abc"})}
	st, err := reportKept(t.Context(), cfg, []kept{k}, reportState{}, filepath.Join(dir, "kept.txt"), &printer{w: io.Discard})
	if err == nil {
		t.Fatal("a failed send returned no error")
	}
	if len(st.Reported) != 0 {
		t.Fatalf("reported set advanced on a failed send: %q", st.Reported)
	}
}

func TestKeptTextCapsTheNamedWorkspaces(t *testing.T) {
	t.Parallel()
	var fresh []kept
	for range reportCap + 3 {
		fresh = append(fresh, kept{root: "/w", line: "/w [unpushed] abc first"})
	}
	text := keptText("host", fresh, "/state/kept.txt")
	if got := strings.Count(text, "\n- "); got != reportCap {
		t.Fatalf("named %d workspaces, want %d", got, reportCap)
	}
	if !strings.Contains(text, "\n+3 more") {
		t.Fatalf("missing the overflow count in %q", text)
	}
}

func TestPrimaryRootRefusesNonDirTarget(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	notDir := filepath.Join(base, "store-file")
	mustWrite(t, notDir, "x\n")
	bad := filepath.Join(base, "bad")
	mustWrite(t, filepath.Join(bad, jjDir, "repo"), notDir+"\n")
	if _, err := primaryRoot(bad); err == nil {
		t.Fatal("a store pointer to a regular file was accepted")
	}

	good := filepath.Join(base, "primary", jjDir, "repo")
	mustMkdir(t, good)
	ws := filepath.Join(base, "ws")
	mustWrite(t, filepath.Join(ws, jjDir, "repo"), good+"\n")
	got, err := primaryRoot(ws)
	if err != nil {
		t.Fatalf("a store directory was refused: %v", err)
	}
	if want := filepath.Join(base, "primary"); got != want {
		t.Fatalf("primaryRoot = %q, want %q", got, want)
	}
}

func TestNewestMtimeCountsWorkingCopy(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	old := time.Now().Add(-100 * time.Hour)
	mustWrite(t, filepath.Join(root, "src.go"), "x\n")
	mustWrite(t, filepath.Join(root, jjDir, "repo"), "ptr\n")
	wc := filepath.Join(root, jjDir, "working_copy")
	mustWrite(t, filepath.Join(wc, "tree"), "state\n")
	backdate(t, root, old)
	// A jj command rewrites only working_copy. That must still read as activity.
	recent := time.Now().Add(-time.Hour)
	if err := os.Chtimes(wc, recent, recent); err != nil {
		t.Fatal(err)
	}
	got, err := newestMtime(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Before(recent.Add(-time.Second)) {
		t.Fatalf("newestMtime = %s, ignores working_copy mtime %s", got, recent)
	}
}

func TestDeleteRefusedWhenClockMovedPastStamp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "work.txt"), "edited after the scan\n")
	out := &printer{w: io.Discard}

	stale := workspace{Root: dir, Name: "x", Stamp: time.Now().Add(-time.Hour)}
	if err := deleteWorkspace(t.Context(), stale, true, out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "work.txt")); err != nil {
		t.Fatalf("workspace changed after the scan was deleted: %v", err)
	}

	// A stamp past the last change must reach the store check, so the guard is not simply blocking everything.
	fresh := workspace{Root: dir, Name: "x", Stamp: time.Now().Add(time.Hour)}
	if err := deleteWorkspace(t.Context(), fresh, true, out); err == nil {
		t.Fatal("a stamp past the last change did not pass the activity guard")
	}
}

func TestParseFlags(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", home)
	cases := []struct {
		name      string
		args      []string
		wantRoots []string
		wantErr   bool
	}{
		{name: "default root", args: nil, wantRoots: []string{filepath.Join(home, "agents", "workspaces")}},
		{name: "roots repeat in order", args: []string{"-root", "/a", "-root", "/b"}, wantRoots: []string{"/a", "/b"}},
		{name: "zero idle window refused", args: []string{"-idle", "0s"}, wantErr: true},
		{name: "stray argument refused", args: []string{"stray"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseFlags(tc.args, io.Discard)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %t", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if strings.Join(cfg.roots, ",") != strings.Join(tc.wantRoots, ",") {
				t.Fatalf("roots = %q, want %q", cfg.roots, tc.wantRoots)
			}
		})
	}
}

// Not parallel: it replaces the package-level recheckCwds.
func TestDeleteRefusedWhenProcessEnteredAfterScan(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "work.txt"), "x\n")
	saved := recheckCwds
	t.Cleanup(func() { recheckCwds = saved })
	recheckCwds = func(context.Context) ([]string, error) { return []string{filepath.Join(dir, "sub")}, nil }
	// The stamp passes the activity guard, so without the live recheck the call reaches primaryRoot and errors.
	ws := workspace{Root: dir, Name: "x", Stamp: time.Now().Add(time.Hour)}
	if err := deleteWorkspace(t.Context(), ws, true, &printer{w: io.Discard}); err != nil {
		t.Fatalf("live recheck did not skip the delete: %v", err)
	}
}

func TestReportKeptReplacesKeyPerRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config{apply: true, cotalBin: "true", to: "nemo"}
	old := reportKeyOf(dir, []string{"abc"})
	other := reportKeyOf("/elsewhere", []string{"zzz"})
	k := kept{root: dir, line: "w", key: reportKeyOf(dir, []string{"abc", "def"})}
	st, err := reportKept(t.Context(), cfg, []kept{k}, reportState{Reported: []string{old, other}}, filepath.Join(dir, "kept.txt"), &printer{w: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(st.Reported, ",") != strings.Join([]string{other, k.key}, ",") {
		t.Fatalf("reported = %q, want the old key for %s replaced", st.Reported, dir)
	}
}

func TestWorkspaceRootsDepthAndSkips(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	deep := filepath.Join(root, "owner", "group-ws", "name")
	mustMkdir(t, filepath.Join(deep, jjDir))
	tooDeep := filepath.Join(root, "a", "b", "c", "d")
	mustMkdir(t, filepath.Join(tooDeep, jjDir))
	locked := filepath.Join(root, "locked")
	mustMkdir(t, filepath.Join(locked, "inner"))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(locked, 0o700); err != nil {
			t.Errorf("restore mode of %s for cleanup: %v", locked, err)
		}
	})
	roots, _, skipped, err := workspaceRoots(root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(roots, ",") != deep {
		t.Fatalf("roots = %q, want only the depth-3 checkout %s", roots, deep)
	}
	if os.Geteuid() != 0 && len(skipped) != 1 {
		t.Fatalf("skipped = %v, want the unreadable dir counted", skipped)
	}
	if _, _, _, err := workspaceRoots(filepath.Join(root, "missing")); err == nil {
		t.Fatal("a missing root was not reported")
	}
}

func TestReportLineCapsIDs(t *testing.T) {
	t.Parallel()
	var unpushed []string
	for range reportIDCap + 2 {
		unpushed = append(unpushed, "abc first line")
	}
	line := reportLine(workspace{Root: "/w", Unpushed: unpushed})
	if got := strings.Count(line, "abc"); got != reportIDCap {
		t.Fatalf("named %d changes, want %d: %q", got, reportIDCap, line)
	}
	if !strings.HasSuffix(line, "+2 more") {
		t.Fatalf("missing the overflow count in %q", line)
	}
}
