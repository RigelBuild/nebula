package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/coverage"
	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/inventory"
)

func testRestic(t *testing.T, repo, password string, args ...string) {
	t.Helper()
	arguments := append([]string{"--repo", repo, "--password-file", password}, args...)
	out, err := exec.Command("restic", arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("restic %v: %v: %s", args, err, out)
	}
}

func TestResticOutputRelativeRepo(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "restic")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s' \"$2\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	want, err := filepath.Abs("relative-repo")
	if err != nil {
		t.Fatal(err)
	}
	got, err := resticOutput(coverageOptions{repo: "relative-repo", passwordFile: filepath.Join(dir, "password")}, "ls")
	if err != nil || string(got) != want {
		t.Fatalf("repo = %q, %v; want %q", got, err, want)
	}
}

func TestResticOutputLocalRepo(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "restic")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s' \"$2\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	want, err := filepath.Abs("relative-repo")
	if err != nil {
		t.Fatal(err)
	}
	got, err := resticOutput(coverageOptions{repo: "local:relative-repo", passwordFile: filepath.Join(dir, "password")}, "ls")
	if err != nil || string(got) != "local:"+want {
		t.Fatalf("repo = %q, %v; want %q", got, err, "local:"+want)
	}
}

func TestResticOutputBackendRepo(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "restic")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s' \"$2\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, tt := range []struct {
		name string
		repo string
	}{
		{name: "s3", repo: "s3:https://acct.example/bucket/repo"},
		{name: "rest", repo: "rest:http://127.0.0.1:8000/repo"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resticOutput(coverageOptions{repo: tt.repo, passwordFile: filepath.Join(dir, "password")}, "ls")
			if err != nil || string(got) != tt.repo {
				t.Fatalf("repo = %q, %v; want %q", got, err, tt.repo)
			}
		})
	}
}

// TestStreamedDigestsBadTarKillsRestic: a bad tar header while restic keeps writing must fail fast, not hang or verify.
func TestStreamedDigestsBadTarKillsRestic(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "restic")
	// 512 bytes of 'x' fail the tar header checksum; `yes` then fills the pipe forever.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nhead -c 512 /dev/zero | tr '\\0' x\nexec yes\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	opts := coverageOptions{repo: filepath.Join(dir, "repo"), passwordFile: filepath.Join(dir, "password"), restic: bin}
	type result struct {
		digests map[string][32]byte
		err     error
	}
	done := make(chan result, 1)
	go func() {
		digests, err := streamedDigests(opts, "snap", []verifiedEntry{{Path: "/a"}})
		done <- result{digests, err}
	}()
	select {
	case got := <-done:
		if !errors.Is(got.err, tar.ErrHeader) || got.digests != nil {
			t.Fatalf("streamedDigests = %v, %v; want tar.ErrHeader and no digests", got.digests, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("streamedDigests hung on a bad tar stream")
	}
}

func TestResticHistoryMultipleJournals(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"one":    "{\"event\":\"pass-acked\",\"passId\":\"one\"}\n",
		"two":    "{\"event\":\"pass-stored\",\"passId\":\"two\"}\n",
		"restic": "#!/bin/sh\ncase \" $* \" in\n*' snapshots '*) case \" $* \" in *' --host test '*) echo '[{\"id\":\"one\",\"tags\":[\"pass-one\"]},{\"id\":\"two\",\"tags\":[\"pass-two\"]}]';; *) exit 1;; esac;;\n*) echo '{\"struct_type\":\"node\",\"type\":\"file\",\"path\":\"/file\"}';;\nesac\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	list, err := resticHistory(coverageOptions{repo: dir, scope: "test", passwordFile: filepath.Join(dir, "password"), journals: []string{filepath.Join(dir, "one"), filepath.Join(dir, "two")}})
	if err != nil || len(list) != 2 {
		t.Fatalf("history: %+v, %v", list, err)
	}
}

func TestCoverageRealResticHistory(t *testing.T) {
	fixture := historyFixture(t, "test password", "source")
	checkHistoryCase(t, fixture.args, fixture.line)
	checkUnexplainedCase(t, fixture.args, fixture.deltas[0], fixture.line, fixture.source)
}

// historyCase carries the coverage arguments and the deleted-file line for one history fixture.
type historyCase struct {
	args   []string
	line   inventory.Line
	deltas []string
	source string
}

// historyFixture builds a repo with a file deleted after two pass-tagged snapshots.
// The source directory is named sourceName, and the password file holds passwordBytes.
func historyFixture(t *testing.T, passwordBytes, sourceName string) historyCase {
	t.Helper()
	dir := t.TempDir()
	repo, password := filepath.Join(dir, "repo"), filepath.Join(dir, "password")
	source := filepath.Join(dir, sourceName)
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(password, []byte(passwordBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	testRestic(t, repo, password, "init")
	deleted := filepath.Join(source, "deleted")
	if err := os.WriteFile(deleted, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	testRestic(t, repo, password, "backup", "--host", "test", "--tag", "pass-acked", source)
	testRestic(t, repo, password, "backup", "--host", "test", "--tag", "pass-stored", source)
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}
	testRestic(t, repo, password, "backup", "--tag", "final", source)
	line := inventory.Line{Path: deleted, Type: "file", Class: "normal", Size: 3, SHA256: sha256.Sum256([]byte("old"))}
	row, err := json.Marshal(inventory.DeltaRow{Event: "added", Line: &line})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"final": "", "baseline": "", "delta-acked.jsonl": string(row) + "\n", "delta-stored.jsonl": "{\"event\":\"summary\"}\n", "digests": "{}", "vanished": "", "journal": "{\"event\":\"pass-acked\",\"passId\":\"acked\"}\n{\"event\":\"pass-stored\",\"passId\":\"stored\"}\n"}
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	opts := coverageOptions{final: filepath.Join(dir, "final"), baseline: filepath.Join(dir, "baseline"), deltas: []string{filepath.Join(dir, "delta-acked.jsonl"), filepath.Join(dir, "delta-stored.jsonl")}, scope: "test", snapshot: "latest", repo: repo, passwordFile: password, journals: []string{filepath.Join(dir, "journal")}, digests: filepath.Join(dir, "digests"), vanished: filepath.Join(dir, "vanished")}
	args := []string{"--final", opts.final, "--baseline", opts.baseline, "--deltas", opts.deltas[0], "--deltas", opts.deltas[1], "--scope", opts.scope, "--snapshot", opts.snapshot, "--repo", opts.repo, "--password-file", opts.passwordFile, "--journal", opts.journals[0], "--digests", opts.digests, "--vanished", opts.vanished}
	return historyCase{args: args, line: line, deltas: opts.deltas, source: source}
}

// TestCoverageHistoryArgvAndEnvHideSourceAndPassword checks that no restic child sees the source path or the password.
func TestCoverageHistoryArgvAndEnvHideSourceAndPassword(t *testing.T) {
	const passwordSentinel = "history-password-sentinel-9006"
	const sourceSentinel = "history-source-sentinel-9006"
	fixture := historyFixture(t, passwordSentinel, sourceSentinel)
	real, err := exec.LookPath("restic")
	if err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(t.TempDir(), "calls")
	wrapper := t.TempDir()
	body := "#!/bin/sh\n{ printf 'cmd '; tr '\\000' ' ' < /proc/$$/cmdline; printf '\\n'; cat /proc/$$/environ; printf '\\n'; } >> 'CALLS_LOG'\nexec 'REAL_RESTIC' \"$@\"\n"
	body = strings.NewReplacer("CALLS_LOG", calls, "REAL_RESTIC", real).Replace(body)
	if err := os.WriteFile(filepath.Join(wrapper, "restic"), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapper+string(os.PathListSeparator)+os.Getenv("PATH"))
	checkHistoryCase(t, fixture.args, fixture.line)
	checkUnexplainedCase(t, fixture.args, fixture.deltas[0], fixture.line, fixture.source)
	records, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, lists := 0, 0
	for record := range bytes.SplitSeq(records, []byte{'\n'}) {
		rest, ok := strings.CutPrefix(string(record), "cmd ")
		if !ok {
			continue
		}
		padded := " " + rest + " "
		if strings.Contains(padded, " snapshots ") {
			snapshots++
		}
		if strings.Contains(padded, " ls ") {
			lists++
		}
	}
	t.Logf("healthy-subject: snapshots invocations=%d ls invocations=%d", snapshots, lists)
	if snapshots == 0 || lists == 0 {
		t.Fatalf("expected at least one snapshots and one ls invocation, got snapshots=%d ls=%d", snapshots, lists)
	}
	if bytes.Contains(records, []byte(passwordSentinel)) {
		t.Fatal("password bytes appeared in restic argv or environment")
	}
	if bytes.Contains(records, []byte(sourceSentinel)) {
		t.Fatal("source sentinel appeared in restic argv or environment")
	}
}

func checkHistoryCase(t *testing.T, args []string, line inventory.Line) {
	t.Helper()
	var out bytes.Buffer
	command := newCoverageCmd()
	command.SetOut(&out)
	command.SetArgs(args)
	if err := command.Execute(); err != nil {
		t.Fatalf("coverage: %v: %s", err, out.String())
	}
	var report coverage.Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.History) != 1 || report.History[0].SHA256 != line.SHA256 {
		t.Fatalf("history: %+v", report)
	}
}

func checkUnexplainedCase(t *testing.T, args []string, delta string, line inventory.Line, source string) {
	t.Helper()
	line.Path = filepath.Join(source, "unexplained")
	row, err := json.Marshal(inventory.DeltaRow{Event: "added", Line: &line})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(delta, append(row, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	command := newCoverageCmd()
	command.SetOut(&out)
	command.SetArgs(args)
	if err := command.Execute(); err == nil || !strings.Contains(out.String(), "unexplained") {
		t.Fatalf("expected unexplained loss: %v: %s", err, out.String())
	}
}
