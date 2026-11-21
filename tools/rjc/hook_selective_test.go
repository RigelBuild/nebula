package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSelectiveHooksRejectSkippedTransitiveDependency(t *testing.T) {
	f := fixtureRepo(t)
	t.Setenv("RJC_TEST_MOON_QUERY", `{"tasks":{"a":{"check":{"target":"a:check","deps":[{"target":"a:middle"}]},"middle":{"target":"a:middle","deps":[{"target":"a:test"}]},"test":{"target":"a:test"}}}}`)
	_, err := (localHookRunner{repo: f.repo}).targets(f.ctx, "base", "head", []SkippedTarget{{Target: "a:test"}})
	assertRefusalGate(t, err, "skip-target-has-dependents")
}

func TestSelectiveHooksReuseAndReasonChange(t *testing.T) {
	f := fixtureRepo(t)
	f.repo.profile.HookReports = true
	f.commit(t, "selective.txt")
	head := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	line := []Change{{Bookmark: "test-lane/feature", Tip: head, Base: mainOrigin, Owned: true}}
	store := localPrePush{root: f.repo.stateDir}
	id, err := f.repo.hookIdentity(f.ctx, line[0])
	if err != nil {
		t.Fatal(err)
	}
	id.Result = hookFail
	id.FailedTargets = []string{"a:test"}
	if err := store.Save(id); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "env.json")
	t.Setenv("RJC_TEST_HOOK_ENV", path)
	skip := []SkippedTarget{{Target: "a:test", Reason: "EX-1"}}
	if err := f.repo.checkSkipTargets(f.ctx, line, skip, store); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.runHooks(f.ctx, line, skip, localHookRunner{repo: f.repo}, store); err != nil {
		t.Fatal(err)
	}
	rec, found, err := store.Read(f.ctx, id.Repo, 0, head)
	if err != nil || !found || rec.Result != hookPartial {
		t.Fatalf("partial: %+v %v", rec, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]string
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	if env["targets"] != "a:other" {
		t.Fatalf("targets: %v", env)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.checkSkipTargets(f.ctx, line, skip, store); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.runHooks(f.ctx, line, skip, localHookRunner{repo: f.repo}, store); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("identical partial reran: %v", err)
	}
	skip[0].Reason = "EX-2"
	if err := f.repo.checkSkipTargets(f.ctx, line, skip, store); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.runHooks(f.ctx, line, skip, localHookRunner{repo: f.repo}, store); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("changed reason did not rerun: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	skip[0].Reason = "EX-1"
	if err := f.repo.checkSkipTargets(f.ctx, line, skip, store); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.runHooks(f.ctx, line, skip, localHookRunner{repo: f.repo}, store); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("retained partial reran: %v", err)
	}
}

func TestSubmitReusesIdenticalPartial(t *testing.T) {
	f := fixtureRepo(t)
	f.repo.profile.HookReports = true
	bookmark := f.new(t, "EX-1", "feat(rjc): reuse partial submit", "")
	f.commit(t, "selective.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	head := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	id, err := f.repo.hookIdentity(f.ctx, Change{Bookmark: bookmark, Tip: head, Base: mainOrigin, Owned: true})
	if err != nil {
		t.Fatal(err)
	}
	id.Result = hookFail
	id.FailedTargets = []string{"a:test"}
	if err := (localPrePush{root: f.repo.stateDir}).Save(id); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hook-env.json")
	t.Setenv("RJC_TEST_HOOK_ENV", path)
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): reuse partial submit", bodyFile: f.bodyFile, skippedTargets: []SkippedTarget{{Target: "a:test", Reason: "EX-1"}}}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !f.metadata[f.prs[bookmark]].Draft || !f.drafts[0] {
		t.Fatal("--skip-target did not create a draft PR")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("partial submit reran hooks: %v", err)
	}
	if len(f.pushes) != 2 {
		t.Fatalf("submit transactions: %v", f.pushes)
	}
}

func TestSelectiveGateFailurePreservesAuthorization(t *testing.T) {
	f := fixtureRepo(t)
	f.repo.profile.HookReports = true
	f.commit(t, "selective.txt")
	head := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	line := []Change{{Bookmark: "test-lane/feature", Tip: head, Base: mainOrigin, Owned: true}}
	store := localPrePush{root: f.repo.stateDir}
	id, err := f.repo.hookIdentity(f.ctx, line[0])
	if err != nil {
		t.Fatal(err)
	}
	id.Result = hookFail
	id.FailedTargets = []string{"a:test"}
	if err := store.Save(id); err != nil {
		t.Fatal(err)
	}
	skip := []SkippedTarget{{Target: "a:test", Reason: "EX-1"}}
	t.Setenv("RJC_TEST_HOOK_FAIL", "1")
	t.Setenv("RJC_TEST_GATE_FAIL", "1")
	if err := f.repo.runHooks(f.ctx, line, skip, localHookRunner{repo: f.repo}, store); err == nil {
		t.Fatal("gate failure accepted")
	}
	rec, found, err := store.Read(f.ctx, id.Repo, 0, head)
	if err != nil || !found || rec.Result != hookFail || len(rec.FailedTargets) != 0 || len(rec.FailedGates) != 1 || len(rec.SkippedTargets) != 1 {
		t.Fatalf("gate failure record: %+v %v", rec, err)
	}
	if err := f.repo.checkSkipTargets(f.ctx, line, skip, store); err != nil {
		t.Fatal(err)
	}
}
