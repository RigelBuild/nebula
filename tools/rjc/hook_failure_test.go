package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSetupFailureCannotAuthorizeSkip(t *testing.T) {
	f := fixtureRepo(t)
	f.commit(t, "feature.txt")
	head := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	line := []Change{{Bookmark: "test-lane/feature", Tip: head, Base: mainOrigin, Owned: true}}
	store := localPrePush{root: f.repo.stateDir}
	t.Setenv("RJC_TEST_HOOK_PHASE", "setup-failed")
	err := f.repo.runHooks(f.ctx, line, nil, localHookRunner{repo: f.repo}, store)
	var detail *failure
	if !errors.As(err, &detail) || detail.code != 3 {
		t.Fatalf("setup error: %v", err)
	}
	_, found, err := store.Read(f.ctx, f.repo.profile.Name, 0, head)
	if err != nil || found {
		t.Fatalf("setup wrote evidence: %v %v", found, err)
	}
	stubIssueState(t, f)
	if err := f.repo.checkSkip(f.ctx, line, "main-red:#1", store); err == nil || !strings.Contains(err.Error(), "skip-evidence") {
		t.Fatalf("setup authorized skip: %v", err)
	}
}

func TestFixupLeavesWorkingCopyClean(t *testing.T) {
	f := fixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): add fixup test", "")
	f.commit(t, "feature.txt")
	t.Setenv("RJC_TEST_HOOK_PHASE", "fixup")
	var stdout, stderr bytes.Buffer
	code := run(f.ctx, []string{"submit", bookmark, "--title", "feat(rjc): add fixup test", "--body-file", f.bodyFile}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "jj new "+strings.Repeat("f", 40)) {
		t.Fatalf("fixup CLI: code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	if err := f.repo.clean(f.ctx); err != nil {
		t.Fatalf("working copy changed: %v", err)
	}
}

func TestToolFailedHookPhaseNamesToolAndLog(t *testing.T) {
	f := fixtureRepo(t)
	bookmark := f.new(t, "EX-1", "fix(rjc): report jj-hp tool failures", "")
	f.commit(t, "feature.txt")
	t.Setenv("RJC_TEST_HOOK_PHASE", "tool-failed")
	t.Setenv("RJC_TEST_HOOK_FAIL", "1")
	var stdout, stderr bytes.Buffer
	code := run(f.ctx, []string{"submit", bookmark, "--title", "fix(rjc): report jj-hp tool failures", "--body-file", f.bodyFile}, &stdout, &stderr)
	message := stderr.String()
	if code != 3 || !strings.Contains(message, "jj-hp: tool-failure: reported tool failure (tool-failed phase)") || strings.Contains(message, "unknown hook phase") || strings.Contains(message, "direnv exec .") {
		t.Fatalf("tool-failed CLI: code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	_, logLine, ok := strings.Cut(message, "Full log: ")
	if !ok {
		t.Fatalf("missing full log path: %s", message)
	}
	logPath := strings.TrimSpace(strings.Split(logLine, "\n")[0])
	if logPath == "" {
		t.Fatalf("empty full log path: %s", message)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("full log path %q: %v", logPath, err)
	}
}

func TestToolFailedHookPhaseSuggestsDevShellForMissingHk(t *testing.T) {
	tests := []struct {
		name        string
		processPath string
		childPath   string
		config      string
		wantHint    bool
		withEnvrc   bool
	}{
		{
			name:        "hk missing from child PATH",
			processPath: "process-bin",
			childPath:   "child-bin",
			config:      "some/path/hk.pkl",
			wantHint:    true,
			withEnvrc:   true,
		},
		{
			name:        "hk present in child PATH with envrc",
			processPath: "child-bin",
			childPath:   "process-bin",
			config:      hkConfigName,
			withEnvrc:   true,
		},
		{
			name:        "hk missing from child PATH without envrc",
			processPath: "process-bin",
			childPath:   "child-bin",
			config:      hkConfigName,
		},
		{
			name:        "non-hk config with hk missing",
			processPath: "process-bin",
			childPath:   "child-bin",
			config:      "lefthook.yml",
			withEnvrc:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := fixtureRepo(t)
			processBin := t.TempDir()
			childBin := t.TempDir()
			processHK, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"hk", "jj-hp"} {
				if err := os.Symlink(processHK, filepath.Join(processBin, name)); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(processHK, filepath.Join(childBin, "jj-hp")); err != nil {
				t.Fatal(err)
			}
			if tc.withEnvrc {
				writeTestFile(t, filepath.Join(f.repo.root, ".envrc"), "")
			}
			processPaths := map[string]string{"process-bin": processBin, "child-bin": childBin}
			f.repo.env = []string{"PATH=" + processPaths[tc.childPath]}
			t.Setenv("PATH", processPaths[tc.processPath])
			t.Setenv("RJC_TEST_HOOK_PHASE", "tool-failed")
			t.Setenv("RJC_TEST_HOOK_FAIL", "1")
			t.Setenv("RJC_TEST_HOOK_CONFIG", tc.config)
			_, _, err = (localHookRunner{repo: f.repo}).Run(
				f.ctx,
				strings.Repeat("1", 40),
				strings.Repeat("2", 40),
				nil,
				io.Discard,
			)
			var detail *failure
			if !errors.As(err, &detail) || detail.code != 3 {
				t.Fatalf("tool-failed phase: %v", err)
			}
			gotHint := strings.Contains(err.Error(), "run inside the repo dev shell (direnv exec .)")
			if gotHint != tc.wantHint {
				t.Fatalf("dev-shell hint = %v, want %v: %v", gotHint, tc.wantHint, err)
			}
		})
	}
}

func TestMissingHookReportIsToolFailure(t *testing.T) {
	f := fixtureRepo(t)
	runtimeDir := t.TempDir()
	f.repo.env = append(f.repo.env, "XDG_RUNTIME_DIR="+runtimeDir, "GATE_SLOTS=1")
	f.repo.profile.HookReports = true
	t.Setenv("RJC_TEST_MISSING_REPORT", "1")
	_, _, err := (localHookRunner{repo: f.repo}).Run(f.ctx, strings.Repeat("1", 40), strings.Repeat("2", 40), nil, io.Discard)
	var detail *failure
	if !errors.As(err, &detail) || detail.code != 3 {
		t.Fatalf("missing report: %v", err)
	}
	release, err := acquireLocalGateSlot(f.ctx, []string{"XDG_RUNTIME_DIR=" + runtimeDir, "GATE_SLOTS=1"}, io.Discard)
	if err != nil {
		t.Fatalf("slot was not released after hook failure: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}

func TestHookRunnerReadsCopiedFailureReports(t *testing.T) {
	for _, tc := range []struct {
		file, failed string
		skip         []SkippedTarget
	}{
		{file: "ciReport.json", failed: "a:test"},
		{file: "runReport.json", failed: "a:other", skip: []SkippedTarget{{Target: "a:test", Reason: "EX-1"}}},
	} {
		t.Run(tc.file, func(t *testing.T) {
			f := fixtureRepo(t)
			f.repo.profile.HookReports = true
			path, err := filepath.Abs(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("RJC_TEST_MOON_REPORT_FILE", path)
			t.Setenv("RJC_TEST_HOOK_FAIL", "1")
			t.Setenv("RJC_TEST_GATE_FAIL", "1")
			out, _, err := (localHookRunner{repo: f.repo}).Run(f.ctx, strings.Repeat("1", 40), strings.Repeat("2", 40), tc.skip, io.Discard)
			if err != nil || !slices.Equal(out.FailedTargets, []string{tc.failed}) || !slices.Equal(out.FailedGates, []string{"gate-a"}) {
				t.Fatalf("copied reports: %+v, %v", out, err)
			}
		})
	}
}
