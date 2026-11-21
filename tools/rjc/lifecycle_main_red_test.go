package main

import (
	"strings"
	"testing"
)

func TestLifecycleMainRedTarget(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		code         int
		output       string
	}{
		{name: "failing trunk", status: "failed", code: 0, output: "hand off"},
		{name: "passing trunk", status: "passed", code: 1, output: "not-red-on-main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLifecycleFixture(t)
			l.clear("agent", l.head, "main")
			t.Setenv("RJC_TEST_RECHECK_STATUS", tc.status)
			store := localPrePush{root: l.f.repo.stateDir}
			rec, ok, err := store.Read(l.f.ctx, l.f.repo.profile.Name, 1, l.head)
			if err != nil || !ok {
				t.Fatal(err)
			}
			rec.Result = hookPartial
			rec.SkippedTargets = []SkippedTarget{{Target: "a:test", Reason: "EX-1"}}
			if err = store.Save(rec); err != nil {
				t.Fatal(err)
			}
			code, out := l.command(t, "ready", l.bookmark, "--main-red")
			if code != tc.code || !strings.Contains(out, tc.output) {
				t.Fatalf("code=%d output=%s", code, out)
			}
			if tc.code == 1 && !strings.Contains(out, "a:test") {
				t.Fatalf("missing passing target: %s", out)
			}
			if tc.code == 0 {
				rec, _, err = store.Read(l.f.ctx, l.f.repo.profile.Name, 1, l.head)
				if err != nil || rec.Result != "main-red" || rec.MainTip == "" {
					t.Fatalf("record %#v %v", rec, err)
				}
				code, out = l.command(t, "ready", l.bookmark)
				if code != 0 {
					t.Fatalf("plain ready %d %s", code, out)
				}
				jjTest(t, l.f.repo, "new", mainOrigin)
				l.f.commit(t, "trunk-advance.txt")
				jjTest(t, l.f.repo, "bookmark", "move", "main", "--to", "@-")
				jjTest(t, l.f.repo, "git", "push", "--bookmark", "main")
				code, out = l.command(t, "ready", l.bookmark)
				if code != 1 || !strings.Contains(out, "hooks-not-passed") {
					t.Fatalf("stale trunk %d %s", code, out)
				}
			}
			workspaces := jjTest(t, l.f.repo, "workspace", "list")
			if strings.Contains(workspaces, "rjc-main-red-") {
				t.Fatalf("temporary workspace leaked: %s", workspaces)
			}
		})
	}
}
