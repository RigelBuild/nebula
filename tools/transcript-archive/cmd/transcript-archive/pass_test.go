package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/coverage"
	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/fence"
)

type passFixture struct {
	opts     passOptions
	identity *age.X25519Identity
	session  string
	calls    string
}

// newPassFixture writes a one-source config under a temp HOME and a wrapper restic that logs each call.
func newPassFixture(t *testing.T) passFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	sessions := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(sessions, "one.jsonl")
	if err := os.WriteFile(session, []byte("{\"a\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := "package archive\nhosts: testhost: {\n\tstate: \"measured\"\n\tsources: [{name: \"s\", path: \"sessions\"}]\n\twatch: []\n\texclude: []\n\tfinalOnly: []\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "archive.cue"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	real, err := exec.LookPath("restic")
	if err != nil {
		t.Fatalf("restic must be on PATH: %v", err)
	}
	calls := filepath.Join(dir, "calls")
	wrapper := filepath.Join(dir, "restic-wrapper")
	// The wrapper logs argv and each --include-file list, so tests see exactly which files a restore fetched.
	body := "#!/bin/sh\necho \"$@\" >> '" + calls + "'\nprev=\nfor a in \"$@\"; do\n  if [ \"$prev\" = --include-file ]; then sed 's/^/include /' \"$a\" >> '" + calls + "'; fi\n  prev=$a\ndone\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	opts := passOptions{
		host: "testhost", configPath: filepath.Join(dir, "archive.cue"), stateDir: filepath.Join(dir, "state", "testhost"),
		repo: filepath.Join(dir, "repo"), escrowOut: filepath.Join(dir, "escrow", "testhost.age"),
		escrowRecipient: identity.Recipient().String(), restic: wrapper,
	}
	return passFixture{opts: opts, identity: identity, session: session, calls: calls}
}

func decryptEscrow(t *testing.T, f passFixture) []byte {
	t.Helper()
	data, err := os.ReadFile(f.opts.escrowOut)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := age.Decrypt(bytes.NewReader(data), f.identity)
	if err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	if _, err := plain.ReadFrom(reader); err != nil {
		t.Fatal(err)
	}
	return plain.Bytes()
}

func journalCount(t *testing.T, state, event string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), `"event":"`+event+`"`)
}

func TestPassFirstAndSecondRun(t *testing.T) {
	f := newPassFixture(t)
	t.Setenv("RESTIC_PASSWORD", "wrong-env-password")
	t.Setenv("RESTIC_REPOSITORY", filepath.Join(t.TempDir(), "elsewhere"))
	if err := runPass(t.Context(), f.opts); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	password, err := os.ReadFile(filepath.Join(f.opts.stateDir, "password"))
	if err != nil {
		t.Fatal(err)
	}
	if got := decryptEscrow(t, f); !bytes.Equal(got, password) {
		t.Fatal("escrow does not decrypt to the state password")
	}
	escrowBefore, err := os.Stat(f.opts.escrowOut)
	if err != nil {
		t.Fatal(err)
	}
	if n := journalCount(t, f.opts.stateDir, "pass-acked"); n != 1 {
		t.Fatalf("acked passes after first run = %d", n)
	}
	if err := os.WriteFile(f.session, []byte("{\"a\":1}\n{\"b\":2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runPass(t.Context(), f.opts); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if n := journalCount(t, f.opts.stateDir, "pass-acked"); n != 2 {
		t.Fatalf("acked passes after second run = %d", n)
	}
	escrowAfter, err := os.Stat(f.opts.escrowOut)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(escrowBefore, escrowAfter) || !escrowAfter.ModTime().Equal(escrowBefore.ModTime()) {
		t.Fatal("escrow rewritten on second run")
	}
	calls, err := os.ReadFile(f.calls)
	if err != nil {
		t.Fatalf("--restic wrapper never called: %v", err)
	}
	if !strings.Contains(string(calls), " init") || strings.Count(string(calls), " backup") != 2 {
		t.Fatalf("wrapper calls: %s", calls)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.opts.repo), "elsewhere")); err == nil {
		t.Fatal("RESTIC_REPOSITORY from env was honoured")
	}
	checkPassCoverage(t, f)
}

func TestPassRefusesEscrowForOtherPassword(t *testing.T) {
	f := newPassFixture(t)
	if err := runPass(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(f.opts.escrowOut)
	if err != nil {
		t.Fatal(err)
	}
	other := f.opts
	other.stateDir = filepath.Join(t.TempDir(), "state", "testhost")
	other.repo = filepath.Join(t.TempDir(), "repo")
	if err := runPass(t.Context(), other); err == nil || !strings.Contains(err.Error(), "different password") {
		t.Fatalf("second scope with same escrow-out = %v, want refusal", err)
	}
	after, err := os.ReadFile(f.opts.escrowOut)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("escrow overwritten")
	}
	if _, err := os.Stat(other.repo); err == nil {
		t.Fatal("repository initialized despite escrow refusal")
	}
}

// stateCoverage runs `coverage --state-dir` before a delete.
func stateCoverage(t *testing.T, f passFixture) (coverage.Report, error) {
	t.Helper()
	var out bytes.Buffer
	command := newCoverageCmd()
	command.SetOut(&out)
	command.SetArgs([]string{"--state-dir", f.opts.stateDir, "--scope", f.opts.host, "--repo", f.opts.repo, "--restic", f.opts.restic})
	err := command.Execute()
	var report coverage.Report
	// On failure cobra appends usage after the report; decode the report only.
	if jsonErr := json.NewDecoder(bytes.NewReader(out.Bytes())).Decode(&report); jsonErr != nil {
		t.Fatalf("coverage output %q: %v (run error %v)", out.String(), jsonErr, err)
	}
	return report, err
}

func checkPassCoverage(t *testing.T, f passFixture) {
	t.Helper()
	before := strings.Count(readString(t, f.calls), " restore") + strings.Count(readString(t, f.calls), "dump --no-lock --archive tar")
	report, err := stateCoverage(t, f)
	if err != nil || !report.Pass {
		t.Fatalf("coverage --state-dir = %+v, %v", report, err)
	}
	if after := strings.Count(readString(t, f.calls), " restore") + strings.Count(readString(t, f.calls), "dump --no-lock --archive tar"); after <= before {
		t.Fatal("coverage did not read the snapshot through the --restic binary")
	}
}

// restoredPaths returns the paths coverage asked restic to restore after offset bytes of the call log.
func restoredPaths(t *testing.T, f passFixture, offset int) []string {
	t.Helper()
	var paths []string
	for line := range strings.Lines(readString(t, f.calls)[offset:]) {
		if path, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "include "); ok {
			paths = append(paths, path)
		}
	}
	return paths
}

// TestCoverageStateDirIncremental: a second check restores nothing; after a change only the changed file is restored.
func TestCoverageStateDirIncremental(t *testing.T) {
	f := newPassFixture(t)
	other := filepath.Join(filepath.Dir(f.session), "other.json")
	// Large enough that one changed session stays under half the snapshot, so the include path runs.
	if err := os.WriteFile(other, []byte(strings.Repeat("{\"o\":1}\n", 512)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runPass(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	checkPassCoverage(t, f)
	offset := len(readString(t, f.calls))
	if report, err := stateCoverage(t, f); err != nil || !report.Pass {
		t.Fatalf("second coverage = %+v, %v", report, err)
	}
	if paths := restoredPaths(t, f, offset); len(paths) != 0 {
		t.Fatalf("second coverage restored unchanged files %v", paths)
	}
	if err := os.WriteFile(f.session, []byte("{\"a\":1}\n{\"c\":3}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runPass(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	offset = len(readString(t, f.calls))
	checkPassCoverage(t, f)
	if paths := restoredPaths(t, f, offset); !slices.Equal(paths, []string{f.session}) {
		t.Fatalf("coverage after one change restored %v, want only %s", paths, f.session)
	}
}

// TestCoverageStateDirFirstRunStreams: when every file is unverified, coverage streams the snapshot once instead of include batches.
func TestCoverageStateDirFirstRunStreams(t *testing.T) {
	f := newPassFixture(t)
	// The snapshot also holds the pass's delta file; a real transcript outweighs it, as on a host.
	if err := os.WriteFile(f.session, []byte(strings.Repeat("{\"a\":1}\n", 512)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runPass(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	offset := len(readString(t, f.calls))
	if report, err := stateCoverage(t, f); err != nil || !report.Pass {
		t.Fatalf("first coverage = %+v, %v", report, err)
	}
	calls := readString(t, f.calls)[offset:]
	if !strings.Contains(calls, "dump --no-lock --archive tar") || strings.Contains(calls, " restore") {
		t.Fatalf("first coverage did not stream the snapshot: %s", calls)
	}
}

// TestCoverageStateDirTornJournalTail: coverage skips a torn final journal line and leaves the journal untouched.
func TestCoverageStateDirTornJournalTail(t *testing.T) {
	f := newPassFixture(t)
	if err := runPass(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(f.opts.stateDir, "journal.jsonl")
	torn := readString(t, journal) + `{"event":"pass-ack`
	if err := os.WriteFile(journal, []byte(torn), 0o600); err != nil {
		t.Fatal(err)
	}
	if report, err := stateCoverage(t, f); err != nil || !report.Pass {
		t.Fatalf("coverage with torn journal tail = %+v, %v", report, err)
	}
	if got := readString(t, journal); got != torn {
		t.Fatal("coverage modified the journal")
	}
}

// TestCoverageStateDirPatternNames: on the include path, glob and $VAR names are escaped and a space-edged name is dumped.
func TestCoverageStateDirPatternNames(t *testing.T) {
	f := newPassFixture(t)
	dir := filepath.Dir(f.session)
	// A large verified sibling keeps the new names under half the snapshot, so the include path runs.
	if err := os.WriteFile(filepath.Join(dir, "other.json"), []byte(strings.Repeat("{\"o\":1}\n", 512)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runPass(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	checkPassCoverage(t, f)
	for _, name := range []string{"a[1].jsonl", "x$HOME.jsonl", "q*?.jsonl", "trail .jsonl "} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := runPass(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	offset := len(readString(t, f.calls))
	if report, err := stateCoverage(t, f); err != nil || !report.Pass {
		t.Fatalf("coverage with pattern-like names = %+v, %v", report, err)
	}
	paths := restoredPaths(t, f, offset)
	slices.Sort(paths)
	want := []string{dir + `/a\[1].jsonl`, dir + `/q\*\?.jsonl`, dir + `/x$$HOME.jsonl`}
	slices.Sort(want)
	if !slices.Equal(paths, want) {
		t.Fatalf("include list = %q, want %q", paths, want)
	}
	dumped := false
	for line := range strings.Lines(readString(t, f.calls)[offset:]) {
		line = strings.TrimSuffix(line, "\n")
		if strings.Contains(line, " dump --no-lock ") && !strings.Contains(line, "--archive") && strings.HasSuffix(line, " "+dir+"/trail .jsonl ") {
			dumped = true
		}
	}
	if !dumped {
		t.Fatalf("no single-file dump for the space-edged name: %s", readString(t, f.calls)[offset:])
	}
}

// TestCoverageStateDirCacheBoundToSnapshot: a cached proof must not certify a newer snapshot holding other bytes.
func TestCoverageStateDirCacheBoundToSnapshot(t *testing.T) {
	f := newPassFixture(t)
	if err := runPass(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	checkPassCoverage(t, f)
	original := readString(t, f.session)
	planted := strings.Repeat("z", len(original)-1) + "\n"
	if err := os.WriteFile(f.session, []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	backup := exec.Command(f.opts.restic, "--repo", f.opts.repo, "--password-file", filepath.Join(f.opts.stateDir, "password"), "backup", "--host", f.opts.host, f.session)
	if output, err := backup.CombinedOutput(); err != nil {
		t.Fatalf("plant same-size snapshot: %v: %s", err, output)
	}
	plantAckedSnapshot(t, f)
	if err := os.WriteFile(f.session, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := stateCoverage(t, f)
	if err == nil || report.Pass || !slices.Contains(report.HashMismatch, f.session) {
		t.Fatalf("coverage with cache and planted bytes = %+v, %v", report, err)
	}
}

// TestPassFirstRunManyPendingBoundedJournal: 60k files all pending on the first pass keep every journal line small, and coverage still runs.
func TestPassFirstRunManyPendingBoundedJournal(t *testing.T) {
	f := newPassFixture(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "sessions", "bulk")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range 60000 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-with-a-long-enough-name-%05d.json", i)), []byte{byte(i)}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := runPass(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(filepath.Join(f.opts.stateDir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range bytes.SplitSeq(journal, []byte{'\n'}) {
		if len(line) > 64*1024 {
			t.Fatalf("journal line of %d bytes: %.120s", len(line), line)
		}
	}
	if report, err := stateCoverage(t, f); err != nil || !report.Pass {
		t.Fatalf("coverage after 60k-file first pass = %+v, %v", report.Missing[:min(3, len(report.Missing))], err)
	}
}

// TestCoverageStateDirDetectsArchivedMismatch swaps the archived bytes; coverage must hash the snapshot, not trust the inventory.
func TestCoverageStateDirDetectsArchivedMismatch(t *testing.T) {
	f := newPassFixture(t)
	if err := runPass(t.Context(), f.opts); err != nil {
		t.Fatal(err)
	}
	original := readString(t, f.session)
	if err := os.WriteFile(f.session, []byte("{\"x\":9}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backup := exec.Command(f.opts.restic, "--repo", f.opts.repo, "--password-file", filepath.Join(f.opts.stateDir, "password"), "backup", "--host", f.opts.host, f.session)
	if output, err := backup.CombinedOutput(); err != nil {
		t.Fatalf("plant mismatched snapshot: %v: %s", err, output)
	}
	// Tag-free snapshot is newest but not journaled; make the journal point at it as the acked pass.
	plantAckedSnapshot(t, f)
	if err := os.WriteFile(f.session, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := stateCoverage(t, f)
	if err == nil || report.Pass || !slices.Contains(report.HashMismatch, f.session) {
		t.Fatalf("coverage with mismatched archived bytes = %+v, %v", report, err)
	}
}

// plantAckedSnapshot appends a pass-acked event naming the newest snapshot.
func plantAckedSnapshot(t *testing.T, f passFixture) {
	t.Helper()
	output, err := exec.Command(f.opts.restic, "--repo", f.opts.repo, "--password-file", filepath.Join(f.opts.stateDir, "password"), "snapshots", "--json", "latest").Output()
	if err != nil {
		t.Fatal(err)
	}
	var snapshots []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(output, &snapshots); err != nil || len(snapshots) != 1 {
		t.Fatalf("latest snapshot %s: %v", output, err)
	}
	journal, err := os.OpenFile(filepath.Join(f.opts.stateDir, "journal.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.WriteString(`{"event":"pass-acked","passId":"planted","snapshotId":"` + snapshots[0].ID + "\"}\n"); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
}

func readString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPassRefusesStateUnderSourceRoot(t *testing.T) {
	f := newPassFixture(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	inside := f.opts
	inside.stateDir = filepath.Join(home, "sessions", "state")
	if err := runPass(t.Context(), inside); err == nil || !strings.Contains(err.Error(), "under source") {
		t.Fatalf("state dir under a source root = %v, want refusal", err)
	}
	if _, err := os.Stat(inside.stateDir); err == nil {
		t.Fatal("state dir created despite refusal")
	}
}

// A state dir reached through a symlink into a source root is refused (L1).
func TestPassRefusesSymlinkedStateUnderSourceRoot(t *testing.T) {
	f := newPassFixture(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(filepath.Join(home, "sessions"), link); err != nil {
		t.Fatal(err)
	}
	inside := f.opts
	inside.stateDir = filepath.Join(link, "state")
	if err := runPass(t.Context(), inside); err == nil || !strings.Contains(err.Error(), "under source") {
		t.Fatalf("symlinked state dir under a source root = %v, want refusal", err)
	}
}

// A live holder of the state-dir lock stops a second pass (L3).
func TestPassRefusesLockedStateDir(t *testing.T) {
	f := newPassFixture(t)
	release, err := fence.Lock(t.Context(), f.opts.stateDir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := release(); err != nil {
			t.Error(err)
		}
	}()
	if err := runPass(t.Context(), f.opts); err == nil || !strings.Contains(err.Error(), "lock state directory") {
		t.Fatalf("pass with a held state lock = %v, want refusal", err)
	}
}
