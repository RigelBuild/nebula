package main

import (
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
		t.Fatalf("changed unlanded set not reported: %+v", got)
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
	cfg := config{apply: true, cotalBin: filepath.Join(dir, "no-such-cotal"), to: testRecipient}
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
		fresh = append(fresh, kept{root: "/w", line: "/w [unlanded] abc first"})
	}
	text := keptText("host", fresh, "/state/kept.txt")
	if got := strings.Count(text, "\n- "); got != reportCap {
		t.Fatalf("named %d workspaces, want %d", got, reportCap)
	}
	if !strings.Contains(text, "\n+3 more") {
		t.Fatalf("missing the overflow count in %q", text)
	}
}

func TestScanTreeCountsWorkingCopy(t *testing.T) {
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
	scan, err := scanTree(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if scan.newest.Before(recent.Add(-time.Second)) {
		t.Fatalf("scanTree newest = %s, ignores working_copy mtime %s", scan.newest, recent)
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
		{name: "default root", args: []string{"-to", testRecipient}, wantRoots: []string{filepath.Join(home, "agents", "workspaces")}},
		{name: "roots repeat in order", args: []string{"-to", testRecipient, "-root", "/a", "-root", "/b"}, wantRoots: []string{"/a", "/b"}},
		{name: "zero idle window refused", args: []string{"-to", testRecipient, "-idle", "0s"}, wantErr: true},
		{name: "zero go cache cap refused", args: []string{"-to", testRecipient, "-go-cache-max-gib", "0"}, wantErr: true},
		{name: "overflowing go cache cap refused", args: []string{"-to", testRecipient, "-go-cache-max-gib", "9000000000"}, wantErr: true},
		{name: "stray argument refused", args: []string{"-to", testRecipient, "stray"}, wantErr: true},
		{name: "zero shm age refused", args: []string{"-to", testRecipient, "-shm-age", "0s"}, wantErr: true},
		{name: "shm-only still requires recipient", args: []string{"-shm-only"}, wantErr: true},
		{name: "missing recipient refused", args: nil, wantErr: true},
		{name: "empty recipient refused", args: []string{"-to", ""}, wantErr: true},
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

func TestReportKeptReplacesKeyPerRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := config{apply: true, cotalBin: "true", to: testRecipient}
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
	var unlanded []string
	for range reportIDCap + 2 {
		unlanded = append(unlanded, "abc first line")
	}
	line := reportLine(workspace{Root: "/w", Unlanded: unlanded})
	if got := strings.Count(line, "abc"); got != reportIDCap {
		t.Fatalf("named %d changes, want %d: %q", got, reportIDCap, line)
	}
	if !strings.HasSuffix(line, "+2 more") {
		t.Fatalf("missing the overflow count in %q", line)
	}
}
