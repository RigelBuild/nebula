package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestLifecycleTrackingRedTrunk(t *testing.T) {
	l := newLifecycleFixture(t)
	f := l.f
	jjTest(t, f.repo, "bookmark", "create", "rigel-release", "-r", "main@origin")
	jjTest(t, f.repo, "git", "push", "--bookmark", "rigel-release")
	jjTest(t, f.repo, "new", "main@origin")
	f.commit(t, "mirror-before.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	jjTest(t, f.repo, "new", quoteRev(l.bookmark))
	f.repo.profile.Posture = forkTracking
	f.repo.profile.Trunk = "rigel-release"
	f.repo.profile.Mirror = "main"
	meta := f.metadata[1]
	meta.BaseRef = "rigel-release"
	f.metadata[1] = meta
	l.clear("agent", l.head, "rigel-release")
	store := localPrePush{root: f.repo.stateDir}
	rec, ok, err := store.Read(f.ctx, f.repo.profile.Name, 1, l.head)
	if err != nil || !ok {
		t.Fatal(err)
	}
	rec.Result = hookPartial
	rec.SkippedTargets = []SkippedTarget{{Target: "a:test", Reason: "EX-1"}}
	if err = store.Save(rec); err != nil {
		t.Fatal(err)
	}
	trunk := jjTest(t, f.repo, "log", "--no-graph", "-r", "rigel-release@origin", "-T", "commit_id")
	mirror := jjTest(t, f.repo, "log", "--no-graph", "-r", "main@origin", "-T", "commit_id")
	if trunk == mirror {
		t.Fatal("mirror must differ from the trunk")
	}
	t.Setenv("RJC_TEST_FAIL_AT", trunk)
	code, out := l.command(t, "ready", l.bookmark, "--main-red")
	if code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	rec, _, err = store.Read(f.ctx, f.repo.profile.Name, 1, l.head)
	if err != nil {
		t.Fatal(err)
	}
	if rec.MainTip != trunk {
		t.Fatalf("tip %s want %s", rec.MainTip, trunk)
	}
	jjTest(t, f.repo, "new", "main@origin")
	f.commit(t, "mirror-change.txt")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	ledgerPath, err := (Ledger{root: f.repo.stateDir}).path(f.repo.profile.Name, 1)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	promotions := l.promotions
	code, out = l.command(t, "ready", l.bookmark)
	if code != 0 || !strings.Contains(out, "already-ready") {
		t.Fatalf("%d %s", code, out)
	}
	after, err := os.ReadFile(ledgerPath)
	if err != nil || !bytes.Equal(before, after) || l.promotions != promotions {
		t.Fatalf("already-ready mutated state: %v", err)
	}
}
