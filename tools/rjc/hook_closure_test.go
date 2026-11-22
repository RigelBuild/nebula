package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gateModuleFixture commits a real Go module with a gate runner in the profile's gateRunnerDir that imports
// libs/gatetier, on main. The closure is then that dir plus libs/gatetier, derived by go list rather than hardcoded.
func gateModuleFixture(t *testing.T, f *fixture) {
	t.Helper()
	writeTestFile(t, filepath.Join(f.repo.root, "go.mod"), "module example.test/fixture\n\ngo 1.23\n")
	writeTestFile(t, filepath.Join(f.repo.root, "libs/gatetier/tier.go"), "package gatetier\n\nfunc Tier() string { return \"fast\" }\n")
	writeTestFile(t, filepath.Join(f.repo.root, f.repo.profile.GateRunnerDir, "main.go"), "package main\n\nimport _ \"example.test/fixture/libs/gatetier\"\n\nfunc main() {}\n")
	jjTest(t, f.repo, "commit", "-m", "chore: add gate module fixture")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
}

// The gate runner lives in tools/example-gate, not the default tools/gate-runner, so the closure must come from the
// profile's gateRunnerDir: a hardcoded path fails go list and the refusal names the wrong cause.
// M: a rename out of the gate runner dir must refuse, because the old path is in the diff only with --no-renames.
// M: an edit to a closure library (libs/gatetier) must refuse, because the gate runner builds against it.
func TestInheritRefusesGateClosureChange(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, f *fixture)
	}{
		{name: "rename out of gate-runner", write: func(t *testing.T, f *fixture) {
			t.Helper()
			if err := os.MkdirAll(filepath.Join(f.repo.root, "libs/elsewhere"), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(f.repo.root, f.repo.profile.GateRunnerDir, "main.go"), filepath.Join(f.repo.root, "libs/elsewhere/main.go")); err != nil {
				t.Fatal(err)
			}
			jjTest(t, f.repo, "commit", "-m", "fix: move gate runner out")
		}},
		{name: "edit to closure lib gatetier", write: func(t *testing.T, f *fixture) {
			t.Helper()
			writeTestFile(t, filepath.Join(f.repo.root, "libs/gatetier/tier.go"), "package gatetier\n\nfunc Tier() string { return \"full\" }\n")
			jjTest(t, f.repo, "commit", "-m", "fix: change gatetier")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixtureRepo(t)
			stubIssueState(t, f)
			f.repo.profile.GateRunnerDir = "tools/example-gate"
			gateModuleFixture(t, f)
			store, line := inheritFixtureWith(t, f, tc.write)
			err := f.repo.checkSkip(f.ctx, line, "main-red:#7", store)
			if err == nil || !strings.Contains(err.Error(), "skip-evidence") {
				t.Fatalf("%s was inherited: %v", tc.name, err)
			}
			if strings.Contains(err.Error(), "cannot derive the gate-runner closure") {
				t.Fatalf("%s refused for an underivable closure, not the change: %v", tc.name, err)
			}
		})
	}
}

// M fail-closed: if go list cannot derive the closure, inheritance is refused, not allowed through.
func TestInheritRefusesWhenClosureUnderivable(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	stubIssueState(t, f)
	// No go.mod and no gate runner, so go list fails at the lower tip.
	store, line := inheritFixtureNoModule(t, f, "docs/readme.txt")
	err := f.repo.checkSkip(f.ctx, line, "main-red:#7", store)
	if err == nil || !strings.Contains(err.Error(), "cannot derive the gate-runner closure") {
		t.Fatalf("underivable closure was inherited: %v", err)
	}
}

// A symlinked TMPDIR (macOS /var -> /private/var) must not refuse inheritance. go list reports resolved closure
// dirs, so the workspace root is resolved before the containment check in gateRunnerClosure.
func TestInheritAllowsSymlinkedTempDir(t *testing.T) {
	f := fixtureRepo(t)
	stubIssueState(t, f)
	store, line := inheritFixtureFile(t, f, "docs/readme.txt")
	resolved := t.TempDir()
	link := filepath.Join(t.TempDir(), "tmp-link")
	if err := os.Symlink(resolved, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", link)
	if err := f.repo.checkSkip(f.ctx, line, "main-red:#7", store); err != nil {
		t.Fatalf("unrelated increment refused under a symlinked TMPDIR: %v", err)
	}
}
