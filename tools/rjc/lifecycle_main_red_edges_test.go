package main

import (
	"strings"
	"testing"
)

func TestLifecycleFreeTextSkipPromotesOnPlainReady(t *testing.T) {
	t.Parallel()
	l := newLifecycleFixture(t)
	l.clear("agent", l.head, "main")
	store := localPrePush{root: l.f.repo.stateDir}
	rec, _, err := store.Read(l.f.ctx, l.f.repo.profile.Name, 1, l.head)
	if err != nil {
		t.Fatal(err)
	}
	rec.Result = hookSkipped
	rec.Skip = "flaky runner"
	if err = store.Save(rec); err != nil {
		t.Fatal(err)
	}
	code, out := l.command(t, "ready", l.bookmark, "--main-red")
	if code != 1 || !strings.Contains(out, "hooks-skipped") || l.promotions != 0 {
		t.Fatalf("--main-red: code=%d output=%s promotions=%d", code, out, l.promotions)
	}
	if code, out = l.command(t, "ready", l.bookmark); code != 0 || l.promotions != 1 {
		t.Fatalf("plain ready: code=%d output=%s promotions=%d", code, out, l.promotions)
	}
}

func TestLifecycleMainRedRefusesLocalHooksBeforeWorkspace(t *testing.T) {
	t.Parallel()
	l := newLifecycleFixture(t)
	l.clear("agent", l.head, "main")
	store := localPrePush{root: l.f.repo.stateDir}
	rec, found, err := store.Read(l.f.ctx, l.f.repo.profile.Name, 1, l.head)
	if err != nil || !found {
		t.Fatalf("read hook record: found=%t err=%v", found, err)
	}
	rec.Result = hookSkipped
	rec.Skip = "main-red:EX-1"
	if err := store.Save(rec); err != nil {
		t.Fatal(err)
	}
	l.f.repo.env = append(l.f.repo.env, "RJC_HOOK_LOCAL=refuse", "RJC_HOOK_SSH=")
	workspacesBefore := jjTest(t, l.f.repo, "workspace", "list")

	code, out := l.command(t, "ready", l.bookmark, "--main-red")
	if code != 1 || l.promotions != 0 {
		t.Fatalf("code=%d output=%s promotions=%d", code, out, l.promotions)
	}
	_, fix, hasFix := strings.Cut(out, "\nFix: ")
	fix, _, _ = strings.Cut(fix, "\n")
	if !strings.Contains(out, "local-hooks-refused") || !hasFix {
		t.Fatalf("main-red refusal missing fix guidance: %s", out)
	}
	if strings.Contains(fix, "RJC_HOOK_SSH") {
		t.Fatalf("main-red fix guidance mentions SSH hosts: %q", fix)
	}
	workspacesAfter := jjTest(t, l.f.repo, "workspace", "list")
	if workspacesAfter != workspacesBefore {
		t.Fatalf("refusal changed workspaces: before=%s after=%s", workspacesBefore, workspacesAfter)
	}
}

func TestLifecycleMainRedStaleTrunk(t *testing.T) {
	t.Parallel()
	l := newLifecycleFixture(t)
	l.clear("agent", l.head, "main")
	store := localPrePush{root: l.f.repo.stateDir}
	rec, _, err := store.Read(l.f.ctx, l.f.repo.profile.Name, 1, l.head)
	if err != nil {
		t.Fatal(err)
	}
	rec.Result = "main-red"
	rec.MainTip = strings.Repeat("a", 40)
	if err = store.Save(rec); err != nil {
		t.Fatal(err)
	}
	code, out := l.command(t, "ready", l.bookmark)
	if code != 1 || !strings.Contains(out, "hooks-not-passed") || l.promotions != 0 {
		t.Fatalf("%d %s", code, out)
	}
}

func TestMainRedAbortedIsToolFailure(t *testing.T) {
	t.Parallel()
	l := newLifecycleFixture(t)
	l.clear("agent", l.head, l.f.repo.profile.Trunk)
	store := localPrePush{root: l.f.repo.stateDir}
	rec, found, err := store.Read(l.f.ctx, l.f.repo.profile.Name, 1, l.head)
	if err != nil || !found {
		t.Fatalf("read hook record: %v", err)
	}
	rec.Result = hookPartial
	rec.SkippedTargets = []SkippedTarget{{Target: "a:test", Reason: "EX-1"}}
	if err := store.Save(rec); err != nil {
		t.Fatal(err)
	}
	l.f.repo.env = append(l.f.repo.env, "RJC_TEST_RECHECK_STATUS=aborted")
	code, out := l.command(t, "ready", l.bookmark, "--main-red")
	if code != 3 || l.promotions != 0 {
		t.Fatalf("code=%d output=%s promotions=%d", code, out, l.promotions)
	}
}
