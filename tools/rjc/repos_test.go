package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryProfiles(t *testing.T) {
	for _, tc := range []struct {
		name, origin, posture, trunk, mirror string
		pr                                   bool
	}{
		{name: "tracking", origin: "https://github.com/example/tracked-fork", posture: "tracking", trunk: "rigel-release", mirror: "main", pr: true},
		{name: "master tracking", origin: "https://github.com/example/tracked-master", posture: "tracking", trunk: "rigel-release", mirror: "master", pr: true},
		{name: "owned", origin: "https://github.com/example/owned-fork", posture: "owned", trunk: "main", pr: true},
		{name: "plain", origin: "https://github.com/example/factory", posture: "none", trunk: "main", pr: true},
		{name: "alias", origin: "https://github.com/example/factory-alias", posture: "none", trunk: "main", pr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := ProfileFor(loadExampleConfig(t), tc.origin)
			if err != nil {
				t.Fatal(err)
			}
			if string(profile.Posture) != tc.posture || profile.Trunk != tc.trunk || profile.Mirror != tc.mirror || profile.PR != tc.pr || profile.TrunkRemote() != tc.trunk+"@origin" {
				t.Fatalf("unexpected profile: %+v", profile)
			}
		})
	}
}

func TestTrackingTrunk(t *testing.T) {
	f := fixtureRepo(t)
	profile, err := ProfileFor(loadExampleConfig(t), "https://github.com/example/tracked-fork")
	if err != nil {
		t.Fatal(err)
	}
	f.repo.profile = profile
	if err := f.repo.fetchMain(f.ctx); err == nil || !strings.Contains(err.Error(), "fork-profile-incomplete") {
		t.Fatalf("missing trunk: %v", err)
	}
	jjTest(t, f.repo, "bookmark", "create", "rigel-release", "-r", "main@origin")
	jjTest(t, f.repo, "git", "push", "--bookmark", "rigel-release")
	if err := f.repo.fetchMain(f.ctx); err != nil {
		t.Fatal(err)
	}
	bookmark := f.new(t, "EX-1", "feat(rjc): use release trunk", "")
	f.commit(t, "release.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	line, err := f.repo.line(f.ctx, bookmark, lineOptions{})
	if err != nil || len(line) != 1 || line[0].Base != "rigel-release@origin" {
		t.Fatalf("line = %+v, %v", line, err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(executable, filepath.Join(bin, "jj-vine")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RJC_TEST_VINE_SUCCESS", "1")
	expectVineConfig(t, map[string]string{"jj-vine.defaultBaseBranch": "rigel-release"})
	if err := (vine{repo: f.repo, line: line}).Submit(f.ctx, bookmark, f.bodyFile, false, false, &bytes.Buffer{}); err != nil {
		t.Fatalf("tracking vine config: %v", err)
	}
	if _, err := f.repo.newBase(f.ctx, "other/ex-2-closed"); err == nil || !strings.Contains(err.Error(), "closed-fork-parent") {
		t.Fatalf("closed parent: %v", err)
	}
	opt := submitOptions{bookmark: bookmark, title: "feat(rjc): use release trunk", bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	pr := f.prs[bookmark]
	before := f.metadata[pr]
	before.BaseRef = "main"
	f.metadata[pr] = before
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err == nil || !strings.Contains(err.Error(), "fork-base-mismatch") {
		t.Fatalf("mirror base: %v", err)
	}
	opt.retarget = true
	before.BaseRef = "unrelated"
	f.metadata[pr] = before
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err == nil || !strings.Contains(err.Error(), "base") {
		t.Fatalf("unrelated base: %v", err)
	}
	before.BaseRef = "main"
	f.metadata[pr] = before
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	after := f.metadata[pr]
	if after.BaseRef != "rigel-release" || after.HeadSHA != before.HeadSHA || len(f.reviewCalls) != 2 {
		t.Fatalf("retarget: %+v reviews=%v", after, f.reviewCalls)
	}
}

func TestNewRejectsClosedForkParent(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	profile, err := ProfileFor(loadExampleConfig(t), "https://github.com/example/tracked-fork")
	if err != nil {
		t.Fatal(err)
	}
	f.repo.profile = profile
	jjTest(t, f.repo, "bookmark", "create", "rigel-release", "-r", "main@origin")
	jjTest(t, f.repo, "git", "push", "--bookmark", "rigel-release")
	parent := "other-lane/ex-2-closed"
	f.prs[parent] = 1
	f.merged = map[string]bool{parent: true}
	var stdout, stderr bytes.Buffer
	code := run(f.ctx, []string{"new", "--issue", "EX-3", "--title", "feat(rjc): reject closed parent", "--on", parent}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "closed-fork-parent") {
		t.Fatalf("closed parent: code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
}
