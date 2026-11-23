package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFailedAndSkippedRunsRetry(t *testing.T) {
	t.Parallel()
	for _, skipped := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "skipped"}[skipped], func(t *testing.T) {
			t.Parallel()
			f := parallelFixtureRepo(t)
			bookmark := f.new(t, "EX-1", "feat(rjc): retry failed hooks", "")
			f.commit(t, "feature.txt")
			path := filepath.Join(t.TempDir(), "hook-env.json")
			f.repo.env = append(f.repo.env, "RJC_TEST_HOOK_ENV="+path, "RJC_TEST_HOOK_FAIL=1")
			opt := submitOptions{bookmark: bookmark, title: "feat(rjc): retry failed hooks", bodyFile: f.bodyFile}
			if err := f.repo.submit(f.ctx, opt, f, io.Discard); err == nil || !strings.Contains(err.Error(), "hooks-failed") {
				t.Fatalf("failure accepted: %v", err)
			}
			stubIssueState(t, f)
			if skipped {
				opt.skip = "main-red:#1"
				if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
					t.Fatal(err)
				}
				if !f.metadata[f.prs[bookmark]].Draft || !f.drafts[0] {
					t.Fatal("main-red did not create a draft PR")
				}
			}
			line, err := f.repo.line(f.ctx, bookmark, lineOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.repo.checkSkip(f.ctx, line, "main-red:#1", localPrePush{root: f.repo.stateDir}); err != nil {
				t.Fatalf("failure no longer authorizes skip: %v", err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			f.repo.env = append(f.repo.env, "RJC_TEST_HOOK_FAIL=0")
			opt.skip = ""
			if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("ordinary submit did not rerun hooks: %v", err)
			}
		})
	}
}

func TestSkipRejectsStaleIdentity(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	f.commit(t, "feature.txt")
	head := jjTest(t, f.repo, "log", "--no-graph", "-r", "@-", "-T", "commit_id")
	line := []Change{{Bookmark: "test-lane/feature", Tip: head, Base: mainOrigin, Owned: true}}
	store := localPrePush{root: f.repo.stateDir}
	id, err := f.repo.hookIdentity(f.ctx, line[0])
	if err != nil {
		t.Fatal(err)
	}
	id.Result = hookFail
	id.HookConfigBlob = strings.Repeat("a", 40)
	if err := store.Save(id); err != nil {
		t.Fatal(err)
	}
	stubIssueState(t, f)
	if err := f.repo.checkSkip(f.ctx, line, "main-red:#1", store); err == nil || !strings.Contains(err.Error(), "skip-evidence") {
		t.Fatalf("stale config accepted: %v", err)
	}
}
