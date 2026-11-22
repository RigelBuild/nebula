package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const p0Reason = "p0:EX-1"

var exampleRootChecks = []string{"biome", "markdownlint", "project-registry"}

// fakeRootChecks records each root-checks run and returns its canned result.
type fakeRootChecks struct {
	roots  []string
	names  [][]string
	result rootChecksResult
	err    error
}

func (f *fakeRootChecks) run(_ context.Context, root string, names []string) (rootChecksResult, error) {
	f.roots = append(f.roots, root)
	f.names = append(f.names, slices.Clone(names))
	data, err := os.ReadFile(filepath.Join(root, "hk.pkl"))
	if err != nil || string(data) != "committed\n" {
		return rootChecksResult{}, errors.New("head checkout lacks the committed file")
	}
	return f.result, f.err
}

// p0Submit submits one change with the given skip and profile skipChecks, using the fake root-checks runner.
func p0Submit(t *testing.T, skip string, skipChecks []string, fake *fakeRootChecks) (*fixture, *bytes.Buffer, error) {
	t.Helper()
	f := fixtureRepo(t)
	f.repo.profile.SkipChecks = skipChecks
	f.repo.rootChecks = fake.run
	f.repo.lookupLinearPriority = func(context.Context, string) (int, string, bool, error) { return 1, "started", true, nil }
	var log bytes.Buffer
	bookmark := f.new(t, "EX-1", "feat(rjc): run root checks", "")
	f.commit(t, "hk.pkl")
	err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): run root checks", bodyFile: f.bodyFile, skip: skip}, f, &log)
	return f, &log, err
}

func TestP0SkipRunsRootChecksOnHeadCheckout(t *testing.T) {
	fake := &fakeRootChecks{}
	f, log, err := p0Submit(t, p0Reason, exampleRootChecks, fake)
	if err != nil || len(f.pushes) != 1 {
		t.Fatalf("p0 skip with passing checks: err=%v pushes=%v", err, f.pushes)
	}
	if len(fake.names) != 1 || !slices.Equal(fake.names[0], exampleRootChecks) {
		t.Fatalf("root checks ran %v, want one run of %v", fake.names, exampleRootChecks)
	}
	if fake.roots[0] == f.repo.root {
		t.Fatal("root checks ran in the workspace, not in a head checkout")
	}
	if _, err := os.Stat(fake.roots[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("head checkout not removed: %v", err)
	}
	if !strings.Contains(log.String(), "root-checks: biome,markdownlint,project-registry passed") {
		t.Fatalf("pass line missing from log: %s", log.String())
	}
}

func TestP0SkipRefusesFailingRootChecks(t *testing.T) {
	fake := &fakeRootChecks{result: rootChecksResult{code: 1, output: "biome: src/app.ts formatter would have printed changes\n"}}
	f, _, err := p0Submit(t, p0Reason, exampleRootChecks, fake)
	var fail *failure
	if !errors.As(err, &fail) || fail.stage != "hooks" || fail.gate != rootChecksGate {
		t.Fatalf("failing root checks: err=%v, want a hooks refusal gated by %s", err, rootChecksGate)
	}
	if !strings.Contains(fail.reason, "formatter would have printed changes") || !strings.Contains(fail.fix, "biome check --write") {
		t.Fatalf("refusal lacks check output or fix hint: reason=%q fix=%q", fail.reason, fail.fix)
	}
	if len(f.pushes) != 0 {
		t.Fatalf("failing root checks still pushed: %v", f.pushes)
	}
}

func TestFreeTextSkipRunsNoRootChecks(t *testing.T) {
	fake := &fakeRootChecks{result: rootChecksResult{code: 1}}
	f, _, err := p0Submit(t, "flaky runner", exampleRootChecks, fake)
	if err != nil || len(f.pushes) != 1 || len(fake.names) != 0 {
		t.Fatalf("free-text skip: err=%v pushes=%v root-check runs=%d", err, f.pushes, len(fake.names))
	}
}

func TestP0SkipWithoutSkipChecksRunsNoRootChecks(t *testing.T) {
	fake := &fakeRootChecks{result: rootChecksResult{code: 1}}
	f, _, err := p0Submit(t, p0Reason, nil, fake)
	if err != nil || len(f.pushes) != 1 || len(fake.names) != 0 {
		t.Fatalf("repo without skipChecks: err=%v pushes=%v root-check runs=%d", err, f.pushes, len(fake.names))
	}
}

func TestProfileListsCheapRootChecks(t *testing.T) {
	profile, err := ProfileFor(loadExampleConfig(t), "https://github.com/example/app.git")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(profile.SkipChecks, exampleRootChecks) {
		t.Fatalf("app skipChecks = %v, want %v", profile.SkipChecks, exampleRootChecks)
	}
	docs, err := ProfileFor(loadExampleConfig(t), "https://github.com/example/docs.git")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs.SkipChecks) != 0 {
		t.Fatalf("docs skipChecks = %v, want none", docs.SkipChecks)
	}
}

// TestRootChecksFixHintCoversEveryFailure checks that the refusal hint names the devenv shell, both fixers, and the
// project-registry expectation file under the profile's rootChecksDir, so it fits any failing check.
func TestRootChecksFixHintCoversEveryFailure(t *testing.T) {
	fix := rootChecksFix("tools/custom-checks")
	for _, want := range []string{"devenv shell", "biome check --write", "rumdl check --fix", "tools/custom-checks/project-registry.expected.json"} {
		if !strings.Contains(fix, want) {
			t.Fatalf("rootChecksFix lacks %q: %s", want, fix)
		}
	}
}

// TestP0SkipNonUrgentRefusedBeforeRootChecks checks that checkSkip refuses a p0 reason whose Linear issue is not
// Urgent before any root-checks run, so the slow checks never start for a skip that cannot push.
func TestP0SkipNonUrgentRefusedBeforeRootChecks(t *testing.T) {
	t.Parallel()
	fake := &fakeRootChecks{}
	f := parallelFixtureRepo(t)
	f.repo.profile.SkipChecks = exampleRootChecks
	f.repo.rootChecks = fake.run
	f.repo.lookupLinearPriority = func(context.Context, string) (int, string, bool, error) { return 2, "started", true, nil }
	bookmark := f.new(t, "EX-1", "feat(rjc): non-urgent p0", "")
	f.commit(t, "hk.pkl")
	err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): non-urgent p0", bodyFile: f.bodyFile, skip: p0Reason}, f, &bytes.Buffer{})
	var fail *failure
	if !errors.As(err, &fail) || fail.gate != "skip-reason" {
		t.Fatalf("non-urgent p0: err=%v, want a skip-reason refusal", err)
	}
	if len(fake.names) != 0 || len(f.pushes) != 0 {
		t.Fatalf("non-urgent p0 ran root checks %v or pushed %v", fake.names, f.pushes)
	}
}

func TestP0SkipInvalidReasonRunsNoRootChecks(t *testing.T) {
	fake := &fakeRootChecks{}
	f, _, err := p0Submit(t, "p0:EX-0", exampleRootChecks, fake)
	var fail *failure
	if !errors.As(err, &fail) || fail.gate != "skip-reason" {
		t.Fatalf("invalid p0 reason: err=%v, want a skip-reason refusal", err)
	}
	if len(fake.names) != 0 || len(f.pushes) != 0 {
		t.Fatalf("invalid p0 reason ran root checks %v or pushed %v", fake.names, f.pushes)
	}
}
