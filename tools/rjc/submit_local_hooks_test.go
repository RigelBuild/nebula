package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/rjc/internal/hookshell"
)

func installFastTrunk(t *testing.T, f *fixture) {
	t.Helper()
	data, err := os.ReadFile("testdata/hk.pkl")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(f.repo.root, "hk.pkl"), string(data))
	jjTest(t, f.repo, "commit", "-m", "fix: fast hook")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
}

func TestSubmitRefusesNonFastLocalHooks(t *testing.T) {
	for _, test := range []struct {
		name  string
		opted bool
		fast  bool
		extra bool
		local string
	}{
		{name: "opted fast head", opted: true, fast: true},
		{name: "opted old head", opted: true},
		{name: "opted extra step", opted: true, fast: true, extra: true},
		{name: "root local override", opted: true, fast: true, local: "hk.local.pkl"},
		{name: "config local override", opted: true, fast: true, local: ".config/hk.local.pkl"},
		{name: "non-opted fast head", fast: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := fixtureRepo(t)
			installFastTrunk(t, f)
			f.repo.profile.LocalFastTier = test.opted
			bookmark := f.new(t, "EX-1", "feat(rjc): allow fast local hook runs", "")
			command := "go run ./tools/gate-runner moon-ci"
			if test.fast {
				command += " --tier fast"
			}
			config := "amends \"package://github.com/jdx/hk/releases/download/v" + hookshell.HKVersion + "/hk@" + hookshell.HKVersion + "#/Config.pkl\"\ndefault_branch = \"main\"\nhooks { [\"pre-push\"] { steps { [\"ci\"] = new Step { check = \"" + command + "\" } [\"gates\"] = new Step { check = \"go run ./tools/gate-runner\" depends = List(\"ci\") } } } }\n"
			if test.extra {
				config = strings.Replace(config, "[\"gates\"]", "[\"other\"] = new Step { check = \"moon ci\" } [\"gates\"]", 1)
			}
			writeTestFile(t, filepath.Join(f.repo.root, "hk.pkl"), config)
			if test.local != "" {
				writeTestFile(t, filepath.Join(f.repo.root, test.local), "override\n")
			}
			jjTest(t, f.repo, "commit", "-m", "fix: committed hook")
			t.Setenv("RJC_HOOK_LOCAL", "refuse")
			t.Setenv("RJC_HOOK_SSH", "")
			var log strings.Builder
			f.repo.log = &log
			before := opLogID(t, f)
			err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): allow fast local hook runs", bodyFile: f.bodyFile}, f, io.Discard)
			if test.opted && test.fast && !test.extra && test.local == "" {
				if err != nil || len(f.pushes) != 1 || !strings.Contains(log.String(), "hooks: runner=local") {
					t.Fatalf("fast submit: err=%v pushes=%v log=%s", err, f.pushes, log.String())
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "local-hooks-refused") || len(f.pushes) != 0 {
					t.Fatalf("expected refusal: err=%v pushes=%v", err, f.pushes)
				}
				if test.opted && opLogID(t, f) != before {
					t.Fatal("refused submit changed operation log")
				}
			}
		})
	}
}

func TestSubmitRefusesLocalHooksBeforeMutation(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry-run-%t", dryRun), func(t *testing.T) {
			f := fixtureRepo(t)
			f.repo.profile.LocalFastTier = false
			bookmark := f.new(t, "EX-1", "feat(rjc): refuse local hook runs", "")
			f.commit(t, "hk.pkl")
			jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
			f.commit(t, "hook-change.txt")
			before := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
			remote, err := f.repo.command(f.ctx, nil, nil, "git", "ls-remote", "origin")
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("RJC_HOOK_LOCAL", "refuse")
			t.Setenv("RJC_HOOK_SSH", "")
			err = f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): refuse local hook runs", bodyFile: f.bodyFile, dryRun: dryRun}, f, io.Discard)
			assertRefusalGate(t, err, "local-hooks-refused")
			detail, ok := errors.AsType[*failure](err)
			if !ok || detail.stage != "hooks" || !strings.Contains(detail.fix, "RJC_HOOK_SSH=user@host[,…]") {
				t.Fatalf("refusal = %v", err)
			}
			if got := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id"); got != before {
				t.Errorf("bookmark moved from %s to %s", before, got)
			}
			after, err := f.repo.command(f.ctx, nil, nil, "git", "ls-remote", "origin")
			if err != nil || !bytes.Equal(remote, after) || len(f.pushes) != 0 {
				t.Fatalf("remote changed: %v pushes=%v", err, f.pushes)
			}
		})
	}
}

func TestSubmitRefusesNewChangeLocalHooks(t *testing.T) {
	f := fixtureRepo(t)
	f.repo.profile.LocalFastTier = false
	f.commit(t, "hk.pkl")
	t.Setenv("RJC_HOOK_LOCAL", "refuse")
	t.Setenv("RJC_HOOK_SSH", "")
	err := f.repo.submit(f.ctx, submitOptions{issue: "EX-1", title: "feat(rjc): refuse local hook runs", bodyFile: f.bodyFile}, f, io.Discard)
	assertRefusalGate(t, err, "local-hooks-refused")
	if got := jjTest(t, f.repo, "bookmark", "list", "--all"); strings.Contains(got, "test-lane/") {
		t.Errorf("refusal left a new bookmark: %s", got)
	}
	if len(f.pushes) != 0 {
		t.Fatalf("refusal pushed: %v", f.pushes)
	}
}

func TestSubmitRefusesOptedAdoptionBeforeMutation(t *testing.T) {
	f := fixtureRepo(t)
	installFastTrunk(t, f)
	f.commit(t, "hk.pkl")
	t.Setenv("RJC_HOOK_LOCAL", "refuse")
	t.Setenv("RJC_HOOK_SSH", "")
	before := opLogID(t, f)
	err := f.repo.submit(f.ctx, submitOptions{issue: "EX-1", title: "feat(rjc): refuse local hook runs", bodyFile: f.bodyFile}, f, io.Discard)
	assertRefusalGate(t, err, "local-hooks-refused")
	if after := opLogID(t, f); before != after {
		t.Fatalf("operation changed: %s -> %s", before, after)
	}
	if got := jjTest(t, f.repo, "bookmark", "list", "--all"); strings.Contains(got, "test-lane/") {
		t.Fatalf("created bookmark: %s", got)
	}
}

func TestSubmitRefusesUnsafeTrunkBeforeMutation(t *testing.T) {
	f := fixtureRepo(t)
	installFastTrunk(t, f)
	bookmark := f.new(t, "EX-1", "feat(rjc): test trunk hook", "")
	f.commit(t, "work.txt")
	jjTest(t, f.repo, "new", mainOrigin)
	f.commit(t, "hk.local.pkl")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	jjTest(t, f.repo, "new", quoteRev(bookmark))
	t.Setenv("RJC_HOOK_LOCAL", "refuse")
	t.Setenv("RJC_HOOK_SSH", "")
	before := opLogID(t, f)
	err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): test trunk hook", bodyFile: f.bodyFile}, f, io.Discard)
	assertRefusalGate(t, err, "local-hooks-refused")
	if after := opLogID(t, f); before != after || len(f.pushes) != 0 {
		t.Fatalf("mutation or push: %s -> %s, %v", before, after, f.pushes)
	}
}

func TestRealHookMatchesFastPolicy(t *testing.T) {
	data, err := os.ReadFile("testdata/hk.pkl")
	if err != nil {
		t.Fatal(err)
	}
	var config strings.Builder
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "//") {
			config.WriteString(line)
			config.WriteByte('\n')
		}
	}
	if !fastHookRE.MatchString(config.String()) {
		t.Fatal("repository hk.pkl does not match fast hook policy")
	}
}

func TestSubmitP0SkipBypassesLocalRefusal(t *testing.T) {
	f := fixtureRepo(t)
	f.repo.profile.LocalFastTier = false
	f.repo.rootChecks = func(context.Context, string, []string) (rootChecksResult, error) { return rootChecksResult{}, nil }
	f.repo.lookupLinearPriority = func(context.Context, string) (int, string, bool, error) { return 1, "started", true, nil }
	bookmark := f.new(t, "EX-1", "feat(rjc): skip local hook", "")
	f.commit(t, "hk.pkl")
	t.Setenv("RJC_HOOK_LOCAL", "refuse")
	t.Setenv("RJC_HOOK_SSH", "")
	err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): skip local hook", bodyFile: f.bodyFile, skip: "p0:EX-1"}, f, io.Discard)
	if err != nil || len(f.pushes) != 1 {
		t.Fatalf("p0 skip: err=%v pushes=%v", err, f.pushes)
	}
}

func TestSubmitLocalHooksWhenPolicyUnset(t *testing.T) {
	f := fixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): allow local hooks", "")
	f.commit(t, "hk.pkl")
	t.Setenv("RJC_HOOK_LOCAL", "")
	t.Setenv("RJC_HOOK_SSH", "")
	var log strings.Builder
	f.repo.log = &log
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: "feat(rjc): allow local hooks", bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.pushes) != 1 || !strings.Contains(log.String(), "hooks: runner=local") {
		t.Fatalf("expected hooks: runner=local and push: log=%s pushes=%v", log.String(), f.pushes)
	}
}
