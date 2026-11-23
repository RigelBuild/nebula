package main

import (
	"strings"
	"testing"
)

func TestLifecycleClosedMainRedReference(t *testing.T) {
	t.Parallel()
	l := newLifecycleFixture(t)
	l.clear("agent", l.head, "main")
	store := localPrePush{root: l.f.repo.stateDir}
	rec, ok, err := store.Read(l.f.ctx, l.f.repo.profile.Name, 1, l.head)
	if err != nil || !ok {
		t.Fatal(err)
	}
	rec.Result = hookSkipped
	rec.Skip = "main-red:#1"
	if err = store.Save(rec); err != nil {
		t.Fatal(err)
	}
	code, out := l.command(t, "ready", l.bookmark, "--main-red")
	if code != 1 || !strings.Contains(out, "skip-reason") || l.promotions != 0 {
		t.Fatalf("%d %s", code, out)
	}
}
