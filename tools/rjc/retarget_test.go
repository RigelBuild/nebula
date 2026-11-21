package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type retargetPRCase struct {
	name                               string
	base, head                         string
	wantError, changeAfter, patchError bool
}

func TestRetargetPR(t *testing.T) {
	for _, tc := range []retargetPRCase{
		{name: "matching snapshot", base: "main", head: "head"},
		{name: "changed base", base: "other", head: "head", wantError: true},
		{name: "changed head", base: "main", head: "other", wantError: true},
		{name: "head changes during patch", base: "main", head: "head", wantError: true, changeAfter: true},
		{name: "patch returns 500", base: "main", head: "head", wantError: true, patchError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, head, patches := tc.base, tc.head, 0
			server := retargetPRTestServer(t, tc, &base, &head, &patches)
			defer server.Close()
			ctx := withServices(t.Context(), &services{forge: &forgeClient{baseURL: server.URL, client: server.Client()}})
			patched := false
			got, err := retargetPR(ctx, "example/fork", 1, "main", "rigel-release", "head", &patched)
			assertRetargetPRResult(t, tc, got, err, patched, patches)
		})
	}
}

func retargetPRTestServer(t *testing.T, tc retargetPRCase, base, head *string, patches *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPatch {
			*patches++
			var body map[string]string
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body) != 1 || body["base"] != "rigel-release" {
				t.Errorf("patch = %v", body)
			}
			*base = body["base"]
			if tc.changeAfter {
				*head = "other"
			}
			if tc.patchError {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		respondJSON(t, w, map[string]any{"head": map[string]string{"sha": *head}, "base": map[string]string{"ref": *base}})
	}))
}

func assertRetargetPRResult(t *testing.T, tc retargetPRCase, got Metadata, err error, patched bool, patches int) {
	t.Helper()
	if patched != (patches == 1) {
		t.Fatalf("patched=%v patches=%d", patched, patches)
	}
	if !tc.wantError {
		if err != nil || got.BaseRef != "rigel-release" || got.HeadSHA != "head" || patches != 1 {
			t.Fatalf("got=%+v err=%v patches=%d", got, err, patches)
		}
		return
	}
	if err == nil {
		t.Fatalf("expected error, patches=%d", patches)
	}
	if tc.patchError || tc.changeAfter {
		assertRetargetPostPatchFailure(t, err)
	} else if !strings.Contains(err.Error(), "head-base-changed") {
		t.Fatalf("err=%v", err)
	}
	if (!tc.changeAfter && !tc.patchError && patches != 0) || ((tc.changeAfter || tc.patchError) && patches != 1) {
		t.Fatalf("err=%v patches=%d", err, patches)
	}
}

func assertRetargetPostPatchFailure(t *testing.T, err error) {
	t.Helper()
	detail, ok := errors.AsType[*failure](err)
	if !ok || detail.code != 3 || detail.stage != "retarget" || !strings.Contains(detail.fix, "--retarget") {
		t.Fatalf("post-PATCH failure = %v", err)
	}
}
func TestRetargetSelectionRefusesBeforePatch(t *testing.T) {
	f := fixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): selection before retarget", "")
	f.commit(t, "work.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	f.prs[bookmark] = 1
	f.metadata[1] = Metadata{HeadRef: bookmark, HeadSHA: tip, BaseRef: "other"}
	item := preparedChange{change: Change{Bookmark: bookmark, Tip: strings.Repeat("a", 40), Base: mainOrigin}, pr: 1, retarget: true, before: f.metadata[1]}
	err := f.repo.pushChange(f.ctx, &item, submitOptions{}, f, io.Discard)
	assertRefusalGate(t, err, "changed-line")
	if f.metadata[1].BaseRef != "other" || len(f.pushes) != 0 {
		t.Fatal("selection refusal mutated the PR or pushed")
	}
}

func TestRestoreRetargetFailureIsToolFailure(t *testing.T) {
	f := fixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): failed base restore", "")
	f.commit(t, "work.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", bookmark)
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	f.prs[bookmark] = 1
	f.metadata[1] = Metadata{HeadRef: bookmark, HeadSHA: tip, BaseRef: "other"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPatch && f.metadata[1].BaseRef == f.repo.profile.Trunk {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.serve(t, w, req)
	}))
	defer server.Close()
	s, err := service(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.forge = &forgeClient{baseURL: server.URL, client: server.Client()}
	f.pushError = errors.New("push failed")
	item := preparedChange{change: Change{Bookmark: bookmark, Tip: tip, Base: mainOrigin}, pr: 1, retarget: true, before: f.metadata[1]}
	err = f.repo.pushChange(f.ctx, &item, submitOptions{}, f, io.Discard)
	detail, ok := errors.AsType[*failure](err)
	if !ok || detail.code != 3 || !strings.Contains(detail.reason, "PR 1") || !strings.Contains(detail.reason, "base remains on trunk") || !strings.Contains(detail.fix, "rjc submit --retarget") {
		t.Fatalf("restore failure = %v", err)
	}
}

func TestRetargetRestoredBaseSkipsReview(t *testing.T) {
	f := fixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): restored base", "")
	f.commit(t, "work.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", bookmark)
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	jjTest(t, f.repo, "bookmark", "create", "other", "-r", mainOrigin)
	jjTest(t, f.repo, "git", "push", "--bookmark", "other")
	f.prs[bookmark] = 1
	f.metadata[1] = Metadata{HeadRef: bookmark, HeadSHA: tip, BaseRef: "other"}
	f.pushError = errors.New("push failed")
	item := preparedChange{change: Change{Bookmark: bookmark, Tip: tip, Base: mainOrigin}, pr: 1, retarget: true, before: f.metadata[1]}
	err := f.repo.pushChange(f.ctx, &item, submitOptions{}, f, io.Discard)
	if err == nil || f.metadata[1].BaseRef != "other" {
		t.Fatalf("err=%v base=%s", err, f.metadata[1].BaseRef)
	}
	if reviewRequestCount(f.reviewCalls) != 0 {
		t.Fatalf("unexpected retarget review: %v", f.reviewCalls)
	}
}

type failedAfterPush struct{ fixture *fixture }

func (e failedAfterPush) Submit(ctx context.Context, bookmark, body string, noHooks, draft bool, log io.Writer) error {
	if err := e.fixture.Submit(ctx, bookmark, body, noHooks, draft, log); err != nil {
		return err
	}
	return errors.New("failed after push")
}

func TestPushErrorAfterHeadMovesRequestsReview(t *testing.T) {
	f := fixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): review failed push", "")
	jjTest(t, f.repo, "git", "push", "--bookmark", bookmark)
	old := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	f.prs[bookmark] = 1
	f.metadata[1] = Metadata{HeadRef: bookmark, HeadSHA: old, BaseRef: "other"}
	f.commit(t, "next.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	item := preparedChange{change: Change{Bookmark: bookmark, Tip: tip, Base: mainOrigin}, pr: 1, retarget: true, before: f.metadata[1]}
	item.state.Coverage = Uncovered
	err := f.repo.pushChange(f.ctx, &item, submitOptions{}, failedAfterPush{fixture: f}, io.Discard)
	detail, ok := errors.AsType[*failure](err)
	if !ok || detail.code != 3 || f.metadata[1].BaseRef != f.repo.profile.Trunk || reviewRequestCount(f.reviewCalls) != 1 {
		t.Fatalf("push error = %v, base %s, reviews %v", err, f.metadata[1].BaseRef, f.reviewCalls)
	}
}

func TestRetargetReadbackFailureKeepsTrunk(t *testing.T) {
	f := fixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): failed push readback", "")
	jjTest(t, f.repo, "git", "push", "--bookmark", bookmark)
	old := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	f.prs[bookmark] = 1
	f.metadata[1] = Metadata{HeadRef: bookmark, HeadSHA: old, BaseRef: "other"}
	f.commit(t, "next.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	failed := false
	f.afterPush = func() { failed = true }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if failed && req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls/1") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.serve(t, w, req)
	}))
	defer server.Close()
	s, err := service(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.forge = &forgeClient{baseURL: server.URL, client: server.Client()}
	item := preparedChange{change: Change{Bookmark: bookmark, Tip: tip, Base: mainOrigin}, pr: 1, retarget: true, before: f.metadata[1]}
	item.state.Coverage = Uncovered
	err = f.repo.pushChange(f.ctx, &item, submitOptions{}, f, io.Discard)
	detail, ok := errors.AsType[*failure](err)
	if !ok || detail.code != 3 || f.metadata[1].BaseRef != f.repo.profile.Trunk || !strings.Contains(detail.fix, "base stays on trunk") || reviewRequestCount(f.reviewCalls) != 1 {
		t.Fatalf("readback error = %v, base %s, reviews %v", err, f.metadata[1].BaseRef, f.reviewCalls)
	}
}

func TestRetargetEarlyExitRequestsReview(t *testing.T) {
	f := fixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): review early exit", "")
	jjTest(t, f.repo, "git", "push", "--bookmark", bookmark)
	old := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	f.prs[bookmark] = 1
	f.metadata[1] = Metadata{HeadRef: bookmark, HeadSHA: old, BaseRef: "other"}
	f.commit(t, "next.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	f.afterPush = func() { m := f.metadata[1]; m.BaseRef = "unexpected"; f.metadata[1] = m }
	item := preparedChange{change: Change{Bookmark: bookmark, Tip: tip, Base: mainOrigin}, pr: 1, retarget: true, before: f.metadata[1]}
	item.state.Coverage = Covered
	err := f.repo.pushChange(f.ctx, &item, submitOptions{}, f, io.Discard)
	if err == nil || reviewRequestCount(f.reviewCalls) != 1 {
		t.Fatalf("error %v, review calls %v", err, f.reviewCalls)
	}
}

func TestRetargetAlreadyOnTrunkRequestsReview(t *testing.T) {
	for _, failAfterPush := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "push-error"}[failAfterPush], func(t *testing.T) {
			f := fixtureRepo(t)
			bookmark := f.new(t, "EX-1", "feat(rjc): review moved head", "")
			jjTest(t, f.repo, "git", "push", "--bookmark", bookmark)
			old := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
			f.prs[bookmark] = 1
			f.metadata[1] = Metadata{HeadRef: bookmark, HeadSHA: old, BaseRef: f.repo.profile.Trunk}
			f.commit(t, "next.txt")
			jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
			tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
			item := preparedChange{change: Change{Bookmark: bookmark, Tip: tip, Base: mainOrigin}, pr: 1, retarget: true, before: f.metadata[1]}
			item.state.Coverage = Covered
			var engine JJVine = f
			if failAfterPush {
				engine = failedAfterPush{fixture: f}
			}
			err := f.repo.pushChange(f.ctx, &item, submitOptions{}, engine, io.Discard)
			if failAfterPush && err == nil {
				t.Fatal("expected push error")
			}
			count := reviewRequestCount(f.reviewCalls)
			if count != 1 {
				t.Fatalf("retarget review count=%d, calls=%v, err=%v", count, f.reviewCalls, err)
			}
		})
	}
}

func TestPushFailureFetchFallsBackToForge(t *testing.T) {
	f := fixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): review despite fetch failure", "")
	jjTest(t, f.repo, "git", "push", "--bookmark", bookmark)
	old := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	f.prs[bookmark] = 1
	f.metadata[1] = Metadata{HeadRef: bookmark, HeadSHA: old, BaseRef: "other"}
	f.commit(t, "next.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	installJJProxy(t)
	f.afterPush = func() { t.Setenv("RJC_TEST_FAIL_FETCH", "1") }
	item := preparedChange{change: Change{Bookmark: bookmark, Tip: tip, Base: mainOrigin}, pr: 1, retarget: true, before: f.metadata[1]}
	item.state.Coverage = Covered
	err := f.repo.pushChange(f.ctx, &item, submitOptions{}, failedAfterPush{fixture: f}, io.Discard)
	if err == nil || reviewRequestCount(f.reviewCalls) != 1 {
		t.Fatalf("error %v, review calls %v", err, f.reviewCalls)
	}
}

func reviewRequestCount(calls []string) int {
	count := 0
	for _, call := range calls {
		if strings.Contains(call, "/requested_reviewers ") {
			count++
		}
	}
	return count
}

func TestRetargetPatchAppliedBeforeError(t *testing.T) {
	f := fixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): ambiguous patch", "")
	f.commit(t, "work.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	f.prs[bookmark] = 1
	f.metadata[1] = Metadata{HeadRef: bookmark, HeadSHA: tip, BaseRef: "other"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPatch {
			m := f.metadata[1]
			m.BaseRef = f.repo.profile.Trunk
			f.metadata[1] = m
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.serve(t, w, req)
	}))
	defer server.Close()
	s, err := service(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.forge = &forgeClient{baseURL: server.URL, client: server.Client()}
	item := preparedChange{change: Change{Bookmark: bookmark, Tip: tip, Base: mainOrigin}, pr: 1, retarget: true, before: f.metadata[1]}
	err = f.repo.pushChange(f.ctx, &item, submitOptions{}, f, io.Discard)
	detail, ok := errors.AsType[*failure](err)
	if !ok || detail.code != 3 || detail.stage != "retarget" || !strings.Contains(detail.fix, "--retarget") || f.metadata[1].BaseRef != f.repo.profile.Trunk || reviewRequestCount(f.reviewCalls) != 1 {
		t.Fatalf("err=%v base=%s reviews=%v", err, f.metadata[1].BaseRef, f.reviewCalls)
	}
}
func TestRetargetReviewOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name                                                       string
		retarget, patch, failPush, failRead, changeAfter, mismatch bool
		want                                                       int
	}{
		{"unpatched-readback-and-fetch-fail", true, false, false, true, false, false, 1},
		{"patched-push-and-fetch-fail", true, true, true, false, false, false, 1},
		{"patch-readback-changed", true, true, false, false, true, false, 1},
		{"patched-success", true, true, false, false, false, false, 1},
		{"pre-patch-base-mismatch", true, true, false, false, false, true, 0},
		{"ordinary-push", false, false, false, false, false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixtureRepo(t)
			bookmark := f.new(t, "EX-1", "feat(rjc): retarget outcomes", "")
			jjTest(t, f.repo, "git", "push", "--bookmark", bookmark)
			old := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
			f.prs[bookmark] = 1
			base := f.repo.profile.Trunk
			if tc.patch {
				base = "other"
			}
			f.metadata[1] = Metadata{HeadRef: bookmark, HeadSHA: old, BaseRef: base}
			f.commit(t, "next.txt")
			jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
			tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
			installJJProxy(t)
			if tc.failPush {
				f.pushError = errors.New("push failed")
				t.Setenv("RJC_TEST_FAIL_FETCH", "1")
			}
			pushed := false
			f.afterPush = func() {
				pushed = true
				if tc.failRead {
					t.Setenv("RJC_TEST_FAIL_FETCH", "1")
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if tc.failRead && pushed && req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls/1") {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				f.serve(t, w, req)
				if tc.changeAfter && req.Method == http.MethodPatch {
					m := f.metadata[1]
					m.HeadSHA = "changed"
					f.metadata[1] = m
				}
			}))
			defer server.Close()
			s, err := service(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			s.forge = &forgeClient{baseURL: server.URL, client: server.Client()}
			item := preparedChange{change: Change{Bookmark: bookmark, Tip: tip, Base: mainOrigin}, pr: 1, retarget: tc.retarget, before: f.metadata[1]}
			if tc.mismatch {
				item.before.BaseRef = "mismatch"
			}
			if tc.retarget {
				item.state.Coverage = Covered
			}
			err = f.repo.pushChange(f.ctx, &item, submitOptions{}, f, io.Discard)
			count := reviewRequestCount(f.reviewCalls)
			if count != tc.want {
				t.Fatalf("retarget reviews=%d want=%d calls=%v err=%v", count, tc.want, f.reviewCalls, err)
			}
		})
	}
}
