package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testRecipient is the report recipient every run test passes with -to, since the flag is required.
const testRecipient = "report-recipient"

// fakeBin writes an executable shell script named name into a fresh dir and returns that dir.
func fakeBin(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// recordingCotal is a fake cotal that writes its arguments, one per line, to the returned file.
func recordingCotal(t *testing.T) (bin, argsFile string) {
	t.Helper()
	argsFile = filepath.Join(t.TempDir(), "args")
	dir := fakeBin(t, "cotal", "printf '%s\\n' \"$@\" > '"+argsFile+"'\n")
	return filepath.Join(dir, "cotal"), argsFile
}

// isolateHome points HOME and the cache and state dirs at a temp dir, so run never prunes the host's real caches.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	return home
}

func TestFailedApplyRunSendsFailureDM(t *testing.T) {
	home := isolateHome(t)
	cotal, argsFile := recordingCotal(t)
	missing := filepath.Join(home, "no-such-root")
	var stderr bytes.Buffer
	code := run(t.Context(), []string{"-apply", "-root", missing, "-cotal", cotal, "-to", testRecipient}, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 for a missing explicit root; stderr %q", code, stderr.String())
	}
	sent, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("no failure DM sent: %v", err)
	}
	if !strings.Contains(string(sent), "failed") || !strings.Contains(string(sent), missing) {
		t.Fatalf("failure DM %q does not name the failure", sent)
	}
}

func TestDryRunFailureSendsNoDM(t *testing.T) {
	home := isolateHome(t)
	cotal, argsFile := recordingCotal(t)
	code := run(t.Context(), []string{"-root", filepath.Join(home, "no-such-root"), "-cotal", cotal, "-to", testRecipient}, &bytes.Buffer{}, &bytes.Buffer{})
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if _, err := os.Stat(argsFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a dry run sent a DM: stat err = %v", err)
	}
}

func TestMissingDefaultRootIsSkipped(t *testing.T) {
	isolateHome(t)
	cotal, argsFile := recordingCotal(t)
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"-apply", "-cotal", cotal, "-to", testRecipient}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0 with no default root; stderr %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "missing, skipped") {
		t.Fatalf("missing default root not logged: %q", stdout.String())
	}
	if _, err := os.Stat(argsFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a clean run sent a DM: stat err = %v", err)
	}
}

func TestFailureTextCapsLength(t *testing.T) {
	t.Parallel()
	short := failureText("host", errors.New("boom"))
	if short != "dev-hygiene on host failed: boom" {
		t.Fatalf("failureText = %q", short)
	}
	long := failureText("host", errors.New(strings.Repeat("x", 5000)))
	if len(long) > 1500+len(" …") || !strings.HasSuffix(long, " …") {
		t.Fatalf("long failure text not capped: %d bytes", len(long))
	}
}

func TestCredsPassedUnresolvedToCotal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	link := filepath.Join(dir, "creds-link")
	// Parsing must not require the creds file: sops may place it after the agent starts.
	cfg, err := parseFlags([]string{"-creds", link, "-state-dir", dir, "-to", testRecipient}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parse failed on a not-yet-present creds file: %v", err)
	}
	if err := sendDM(t.Context(), cfg, "x"); err == nil {
		t.Fatal("send with a missing creds file returned no error")
	}
	target := filepath.Join(dir, "creds-real")
	mustWrite(t, target, "creds\n")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	cotal, argsFile := recordingCotal(t)
	cfg.cotalBin = cotal
	if err := sendDM(t.Context(), cfg, "x"); err != nil {
		t.Fatal(err)
	}
	sent, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sent), "--creds\n"+link+"\n") {
		t.Fatalf("cotal args %q do not carry the creds symlink %s as given", sent, link)
	}
}

// TestInspectRestoresClockOnError checks a failed jj call still restores .jj/working_copy. The fake jj answers the
// workspace list, then touches working_copy and fails, as a snapshot that errors after writing would.
func TestInspectRestoresClockOnError(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wc := filepath.Join(root, jjDir, "working_copy")
	mustMkdir(t, wc)
	old := time.Now().Add(-100 * time.Hour)
	if err := os.Chtimes(wc, old, old); err != nil {
		t.Fatal(err)
	}
	bin := fakeBin(t, "jj", `case "$*" in
*"workspace list"*) printf 'ws\t%s\n' '`+root+`' ;;
*) touch .jj/working_copy/bump; exit 1 ;;
esac
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ws := workspace{Root: root}
	reason, err := inspectWorkspace(t.Context(), &ws, treeScan{})
	if err != nil || reason == "" {
		t.Fatalf("fake jj failure not reported as skipped: reason %q, err %v", reason, err)
	}
	info, err := os.Stat(wc)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().After(old.Add(time.Second)) {
		t.Fatalf("working_copy mtime %s not restored on the error path", info.ModTime())
	}
}

// idleFakeWorkspace makes an idle secondary workspace under a fresh root and a fake jj that logs each argv
// line to the returned file. The fake answers the workspace list and the read-only queries; a snapshotting call
// (no --ignore-working-copy) runs snapshot, so a test picks whether it succeeds.
func idleFakeWorkspace(t *testing.T, snapshot string) (root, dir, argsFile string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(root, "ws")
	mustMkdir(t, filepath.Join(dir, jjDir, "working_copy"))
	// A real store: the orphan check must pass, so the fake jj below is still reached.
	store, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, filepath.Join(store, jjDir, "repo"))
	mustWrite(t, filepath.Join(dir, jjDir, "repo"), filepath.Join(store, jjDir, "repo")+"\n")
	mustWrite(t, filepath.Join(dir, "README.md"), "x\n")
	backdate(t, dir, time.Now().Add(-100*time.Hour))
	argsFile = filepath.Join(t.TempDir(), "jj-args")
	bin := fakeBin(t, "jj", `echo "$*" >> '`+argsFile+`'
case "$*" in
*"workspace list"*) printf 'ws\t%s\n' '`+dir+`' ;;
*--ignore-working-copy*) ;;
*) `+snapshot+` ;;
esac
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return root, dir, argsFile
}

// TestSnapshotErrorSkipsWorkspace checks a workspace jj cannot snapshot is kept, reported skipped, and does not
// fail the run.
func TestSnapshotErrorSkipsWorkspace(t *testing.T) {
	isolateHome(t)
	root, dir, _ := idleFakeWorkspace(t, "echo 'Error: The working copy is stale' >&2; exit 1")
	cotal, _ := recordingCotal(t)
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"-apply", "-root", root, "-cotal", cotal, "-to", testRecipient}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0 with a stale workspace; stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), dir+": skipped: check working copy") {
		t.Fatalf("stale workspace not reported skipped: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "skip jj inspect failed: 1 workspace(s), ") {
		t.Fatalf("skipped workspace not counted: %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		t.Fatalf("skipped workspace was touched: %v", err)
	}
}

// TestSkippedWorkspaceReportedOnce checks a workspace jj cannot inspect is sent to the report recipient as not inspected, once.
func TestSkippedWorkspaceReportedOnce(t *testing.T) {
	isolateHome(t)
	root, dir, _ := idleFakeWorkspace(t, "echo 'Error: The working copy is stale' >&2; exit 1")
	cotal, argsFile := recordingCotal(t)
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"-apply", "-root", root, "-cotal", cotal, "-to", testRecipient}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0; stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	sent, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("skipped workspace not reported: %v", err)
	}
	if !strings.Contains(string(sent), dir+": not inspected (check working copy") {
		t.Fatalf("report %q does not name the skipped workspace", sent)
	}
	if !strings.Contains(string(sent), "not inspected)") {
		t.Fatalf("report header %q does not cover not-inspected workspaces", sent)
	}
	if err := os.Remove(argsFile); err != nil {
		t.Fatal(err)
	}
	if code := run(t.Context(), []string{"-apply", "-root", root, "-cotal", cotal, "-to", testRecipient}, &stdout, &stderr); code != 0 {
		t.Fatalf("second run exit = %d; stderr %q", code, stderr.String())
	}
	if _, err := os.Stat(argsFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("skipped workspace reported twice: stat err = %v", err)
	}
}

// TestSkipReasonIsOneLine checks a multi-line jj error becomes one report line, so one workspace is one list line.
func TestSkipReasonIsOneLine(t *testing.T) {
	home := isolateHome(t)
	root, dir, _ := idleFakeWorkspace(t, "printf 'Error: The working copy is stale\\nHint: Run jj workspace update-stale\\n' >&2; exit 1")
	cotal, _ := recordingCotal(t)
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"-apply", "-root", root, "-cotal", cotal, "-to", testRecipient}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d; stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	list, err := os.ReadFile(filepath.Join(home, "state", "dev-hygiene", "kept.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(list), "\n") {
		if strings.Contains(line, dir) || strings.Contains(line, "Hint:") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "stale; Hint: Run jj workspace update-stale") {
		t.Fatalf("skip reason not one line: %q", lines)
	}
}

// TestEditDuringInspectionBlocksDelete checks the stamp is the pre-jj scan: a file edited while jj runs moves the
// delete recheck past it, so the workspace is kept.
func TestEditDuringInspectionBlocksDelete(t *testing.T) {
	isolateHome(t)
	root, dir, _ := idleFakeWorkspace(t, ":")
	bin := fakeBin(t, "jj", `case "$*" in
*"workspace list"*) printf 'ws\t%s\n' '`+dir+`' ;;
*"file list"*) echo README.md ;;
*--ignore-working-copy*) ;;
*) echo edited > README.md ;;
esac
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"-apply", "-root", root, "-cotal", "false", "-to", testRecipient}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d; stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), dir+": skipped, changed after the run's scan") {
		t.Fatalf("edit during inspection did not block the delete: %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		t.Fatalf("edited workspace was touched: %v", err)
	}
}

// TestReadOnlyQueriesIgnoreWorkingCopy checks every jj query but the dirty check carries --ignore-working-copy, and
// the dirty check does not, so it snapshots unsaved edits.
func TestReadOnlyQueriesIgnoreWorkingCopy(t *testing.T) {
	isolateHome(t)
	root, _, argsFile := idleFakeWorkspace(t, ":")
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"-root", root, "-cotal", "false", "-to", testRecipient}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d; stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	var dirtyChecks, queries int
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		readOnly := strings.Contains(line, "--ignore-working-copy")
		switch {
		case strings.Contains(line, "@ & ~empty()"):
			dirtyChecks++
			if readOnly {
				t.Errorf("dirty check must snapshot, got %q", line)
			}
		case strings.Contains(line, "workspace list"), strings.Contains(line, "file list"), strings.Contains(line, unpushedRevset):
			queries++
			if !readOnly {
				t.Errorf("read-only query lacks --ignore-working-copy: %q", line)
			}
		default:
			t.Errorf("unexpected jj call %q", line)
		}
	}
	if dirtyChecks != 1 || queries != 3 {
		t.Fatalf("jj calls: %d dirty checks, %d queries, want 1 and 3:\n%s", dirtyChecks, queries, raw)
	}
}

// TestMissingWorkspaceDirIsSkipped checks a workspace whose dir vanishes mid-run is reported, and the run goes on.
// ws-a is idle and deleted; ws-b's dir is removed during ws-a's inspection, so it is skipped at its own turn.
func TestMissingWorkspaceDirIsSkipped(t *testing.T) {
	isolateHome(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, filepath.Join(store, jjDir, "repo"))
	dirA := filepath.Join(root, "ws-a")
	dirB := filepath.Join(root, "ws-b")
	mustWrite(t, filepath.Join(dirA, jjDir, "repo"), filepath.Join(store, jjDir, "repo")+"\n")
	mustMkdir(t, filepath.Join(dirA, jjDir, "working_copy"))
	mustWrite(t, filepath.Join(dirA, "README.md"), "x\n")
	backdate(t, dirA, time.Now().Add(-100*time.Hour))
	mustMkdir(t, filepath.Join(dirB, jjDir))
	bin := fakeBin(t, "jj", `case "$*" in
*"workspace list"*root*) printf 'ws-a\t%s\nws-b\t%s\n' '`+dirA+`' '`+dirB+`' ;;
*"workspace list"*) printf 'ws-a\nws-b\n' ;;
*"workspace forget"*) ;;
*"file list"*) rm -rf '`+dirB+`'; echo README.md ;;
*) ;;
esac
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cotal, argsFile := recordingCotal(t)
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"-apply", "-root", root, "-cotal", cotal, "-to", testRecipient}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0 with a vanished workspace dir; stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(dirA); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("idle workspace before the missing one was not deleted: stat err = %v; stdout %q", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "forgotten and deleted") {
		t.Fatalf("idle workspace after the missing one was not processed: stdout %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), dirB+": skipped: directory missing; jj may still list it") {
		t.Fatalf("missing workspace not reported skipped: stdout %q", stdout.String())
	}
	sent, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("missing workspace not reported to the report recipient: %v", err)
	}
	if !strings.Contains(string(sent), dirB+": not inspected (directory missing; jj may still list it)") {
		t.Fatalf("report %q does not name the missing workspace and its reason", sent)
	}
}
