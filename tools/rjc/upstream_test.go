package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func trackingFixture(t *testing.T, mirror string, conflict bool) *fixture {
	t.Helper()
	f := fixtureRepo(t)
	r := f.repo
	r.profile.Posture = forkTracking
	r.profile.Trunk = "rigel-release"
	r.profile.Mirror = mirror
	jjTest(t, r, "bookmark", "create", "rigel-release", "-r", "main@origin")
	jjTest(t, r, "git", "push", "--bookmark", "rigel-release")
	jjTest(t, r, "new", "main@origin")
	file := "upstream.txt"
	if conflict {
		file = "README.md"
	}
	writeTestFile(t, filepath.Join(r.root, file), "upstream\n")
	jjTest(t, r, "commit", "-m", "chore: upstream change")
	if mirror == "main" {
		jjTest(t, r, "bookmark", "move", "main", "--to", "@-")
	} else {
		jjTest(t, r, "bookmark", "create", mirror, "-r", "@-")
	}
	jjTest(t, r, "git", "push", "--bookmark", mirror)
	if conflict {
		jjTest(t, r, "new", "rigel-release@origin")
		writeTestFile(t, filepath.Join(r.root, file), "release\n")
		jjTest(t, r, "commit", "-m", "chore: release change")
		jjTest(t, r, "bookmark", "move", "rigel-release", "--to", "@-")
		jjTest(t, r, "git", "push", "--bookmark", "rigel-release")
	}
	jjTest(t, r, "new", "rigel-release@origin")
	return f
}

func TestUpstreamSyncRejectsNonTracking(t *testing.T) {
	f := fixtureRepo(t)
	for _, posture := range []ForkPosture{forkNone, "owned"} {
		f.repo.profile.Posture = posture
		_, err := f.repo.upstreamSync(f.ctx, upstreamSyncOptions{issue: "EX-1"}, io.Discard)
		assertRefusalGate(t, err, "not-tracking-fork")
	}
	profile, err := ProfileFor(loadExampleConfig(t), "https://github.com/example/docs")
	if err != nil {
		t.Fatal(err)
	}
	f.repo.profile = profile
	_, err = f.repo.upstreamSync(f.ctx, upstreamSyncOptions{issue: "EX-1"}, io.Discard)
	assertRefusalGate(t, err, "not-tracking-fork")
}

func TestUpstreamSyncCreatesRecordedMerge(t *testing.T) {
	for _, mirror := range []string{"main", "master"} {
		t.Run(mirror, func(t *testing.T) {
			f := trackingFixture(t, mirror, false)
			bookmark, err := f.repo.upstreamSync(f.ctx, upstreamSyncOptions{issue: "EX-1"}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			line, err := f.repo.line(f.ctx, bookmark, lineOptions{})
			if err != nil {
				t.Fatal(err)
			}
			before := line[0].Tip
			if _, err := f.repo.rebaseLine(f.ctx, line, false); err != nil {
				t.Fatal(err)
			}
			if after := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id"); after != before {
				t.Fatal("submit preparation rewrote merge")
			}
			rec, ok, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).LoadUpstream(bookmark)
			if err != nil || !ok {
				t.Fatalf("record: %v %v", ok, err)
			}
			identity, err := f.repo.hookIdentity(f.ctx, line[0])
			if err != nil || identity.Base != rec.Release {
				t.Fatalf("hook base %s, want %s: %v", identity.Base, rec.Release, err)
			}
			opt := submitOptions{bookmark: bookmark, title: "chore(fork): merge upstream main " + rec.Upstream[:12], bodyFile: f.bodyFile}
			if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
				t.Fatal(err)
			}
			if f.metadata[1].BaseRef != "rigel-release" || f.metadata[1].HeadSHA != before {
				t.Fatalf("unexpected submitted PR: %+v", f.metadata[1])
			}
			_, err = f.repo.newBase(f.ctx, bookmark)
			assertRefusalGate(t, err, "nonlinear")
		})
	}
}

func TestUpstreamSyncAlreadySynced(t *testing.T) {
	f := trackingFixture(t, "main", false)
	u := jjTest(t, f.repo, "log", "--no-graph", "-r", "rigel-release@origin", "-T", "commit_id")
	bookmark, err := f.repo.upstreamSync(f.ctx, upstreamSyncOptions{issue: "EX-1", upstream: u}, io.Discard)
	if err != nil || bookmark != "" {
		t.Fatalf("already synced: %q %v", bookmark, err)
	}
}

func TestUpstreamSyncConflict(t *testing.T) {
	f := trackingFixture(t, "main", true)
	_, err := f.repo.upstreamSync(f.ctx, upstreamSyncOptions{issue: "EX-1"}, io.Discard)
	assertRefusalGate(t, err, "conflict")
	if !strings.Contains(err.Error(), "README.md") {
		t.Fatalf("missing conflict file: %v", err)
	}
}

func TestUpstreamMergeRejectsUnrecordedAndStacked(t *testing.T) {
	for _, scenario := range []string{"unrecorded", "stacked", "extra commit", "wrong parent", "third parent"} {
		t.Run(scenario, func(t *testing.T) {
			f := trackingFixture(t, "main", false)
			bookmark, err := f.repo.upstreamSync(f.ctx, upstreamSyncOptions{issue: "EX-1"}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			rec, _, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).LoadUpstream(bookmark)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "unrecorded":
				rec.Change = "not-the-change"
			case "stacked", "extra commit":
				writeTestFile(t, filepath.Join(f.repo.root, "stacked.txt"), "stacked\n")
				jjTest(t, f.repo, "commit", "-m", "chore: stacked work")
				if scenario == "stacked" {
					jjTest(t, f.repo, "bookmark", "create", "test-lane/ex-2-stacked", "-r", "@-")
				} else {
					jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
				}
			case "wrong parent":
				rec.Upstream = rec.Release
			case "third parent":
				jjTest(t, f.repo, "new", rec.Release)
				writeTestFile(t, filepath.Join(f.repo.root, "third.txt"), "third\n")
				jjTest(t, f.repo, "commit", "-m", "chore: third parent")
				third := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
				jjTest(t, f.repo, "rebase", "-r", quoteRev(bookmark), "-d", rec.Release, "-d", rec.Upstream, "-d", third)
			}
			if err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).SaveUpstream(bookmark, rec); err != nil {
				t.Fatal(err)
			}
			line := []Change{{Bookmark: bookmark, Tip: jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id"), Base: f.repo.profile.TrunkRemote(), Owned: true}}
			if scenario == "stacked" {
				_, err = f.repo.line(f.ctx, bookmark, lineOptions{})
				assertRefusalGate(t, err, "foreign-descendant")
				return
			}
			merges, err := f.repo.revisions(f.ctx, "merges() & ("+quoteRev(rec.Release)+".."+quoteRev(line[0].Tip)+") ~ ::"+quoteRev("main@origin"))
			if err != nil {
				t.Fatal(err)
			}
			allowed, err := f.repo.allowedMerge(f.ctx, line, merges)
			if err != nil || allowed {
				t.Fatalf("invalid merge accepted: %v %v", allowed, err)
			}
			err = f.repo.checkLinearOwnership(f.ctx, line, false)
			assertRefusalGate(t, err, "nonlinear")
		})
	}
}

func TestUpstreamMergeRejectsSecondParentOutsideMirror(t *testing.T) {
	f := trackingFixture(t, "main", false)
	bookmark, err := f.repo.upstreamSync(f.ctx, upstreamSyncOptions{issue: "EX-1"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	store := fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}
	rec, _, err := store.LoadUpstream(bookmark)
	if err != nil {
		t.Fatal(err)
	}
	jjTest(t, f.repo, "new", rec.Release)
	writeTestFile(t, filepath.Join(f.repo.root, "outside.txt"), "outside mirror\n")
	jjTest(t, f.repo, "commit", "-m", "chore: outside mirror")
	outside := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	jjTest(t, f.repo, "rebase", "-r", quoteRev(bookmark), "-d", rec.Release, "-d", outside)
	rec.Upstream = outside
	if err := store.SaveUpstream(bookmark, rec); err != nil {
		t.Fatal(err)
	}
	inMirror, err := f.repo.revisions(f.ctx, quoteRev(outside)+" & ::"+quoteRev("main@origin"))
	if err != nil || len(inMirror) != 0 {
		t.Fatalf("outside parent is in mirror: %v %v", inMirror, err)
	}
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	line := []Change{{Bookmark: bookmark, Tip: tip, Base: f.repo.profile.TrunkRemote(), Owned: true}}
	allowed, err := f.repo.allowedMergeWithRecord(f.ctx, line, []string{tip}, rec)
	if err != nil || allowed {
		t.Fatalf("merge with outside-mirror second parent accepted: %v %v", allowed, err)
	}
	err = f.repo.checkLinearOwnership(f.ctx, line, false)
	assertRefusalGate(t, err, "nonlinear")
}

func TestUpstreamMergeHooksValidateReleaseDiff(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		fails      bool
	}{
		{name: "changed file fails", file: "upstream.txt", fails: true},
		{name: "unchanged file passes", file: "unrelated.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := trackingFixture(t, "main", false)
			bookmark, err := f.repo.upstreamSync(f.ctx, upstreamSyncOptions{issue: "EX-1"}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			line, err := f.repo.line(f.ctx, bookmark, lineOptions{})
			if err != nil {
				t.Fatal(err)
			}
			rec, _, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).LoadUpstream(bookmark)
			if err != nil {
				t.Fatal(err)
			}
			envPath := filepath.Join(t.TempDir(), "hook-env.json")
			t.Setenv("RJC_TEST_HOOK_ENV", envPath)
			t.Setenv("RJC_TEST_HOOK_FAIL_FILE", tc.file)
			store := localPrePush{root: f.repo.stateDir}
			err = f.repo.runHooks(f.ctx, line, nil, localHookRunner{repo: f.repo}, store)
			if tc.fails {
				assertRefusalGate(t, err, "hooks-failed")
			} else if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(envPath)
			if err != nil {
				t.Fatal(err)
			}
			var env map[string]string
			if err := json.Unmarshal(data, &env); err != nil {
				t.Fatal(err)
			}
			if env["base"] != rec.Release || env["head"] != line[0].Tip {
				t.Fatalf("hook range: %v", env)
			}
			result, found, err := store.Read(f.ctx, f.repo.profile.Name, 0, line[0].Tip)
			want := hookPass
			if tc.fails {
				want = hookFail
			}
			if err != nil || !found || result.Base != rec.Release || result.Result != want {
				t.Fatalf("hook record: %+v %v %v", result, found, err)
			}
		})
	}
}
