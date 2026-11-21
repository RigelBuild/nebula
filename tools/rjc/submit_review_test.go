package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const reviewSubmitTitle = "feat(rjc): request review rounds"

func TestSubmitWithoutReviewDoesNotAddLabel(t *testing.T) {
	f, bookmark := reviewSubmitFixture(t)
	code, output := runReviewSubmit(t, f, bookmark, false, false)
	if code != 0 || f.labelPosts != 0 {
		t.Fatalf("exit=%d label posts=%d output=%s", code, f.labelPosts, output)
	}
	if got := f.metadata[f.prs[bookmark]].Labels; len(got) != 0 {
		t.Fatalf("PR labels = %v, want none", got)
	}
}

func TestSubmitReviewOnCreateAddsLabel(t *testing.T) {
	f, bookmark := reviewSubmitFixture(t)
	code, output := runReviewSubmit(t, f, bookmark, true, false)
	if code != 0 || f.labelPosts != 1 {
		t.Fatalf("exit=%d label posts=%d output=%s", code, f.labelPosts, output)
	}
	if got := f.metadata[f.prs[bookmark]].Labels; !containsReviewRequested(got) {
		t.Fatalf("PR labels = %v, want review-requested", got)
	}

	calls := f.labelPayloads
	if len(calls) != 1 || !slices.Equal(calls[0], []string{"review-requested"}) {
		t.Fatalf("label payloads = %v, want [review-requested]", calls)
	}
}

func TestSubmitReviewSkipsExistingLabel(t *testing.T) {
	f, bookmark := reviewSubmitFixture(t)
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: reviewSubmitTitle, bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	pr := f.prs[bookmark]
	metadata := f.metadata[pr]
	metadata.Labels = []string{"review-requested"}
	f.metadata[pr] = metadata

	code, output := runReviewSubmit(t, f, bookmark, true, false)
	if code != 0 || f.labelPosts != 0 {
		t.Fatalf("exit=%d label posts=%d output=%s", code, f.labelPosts, output)
	}
	if got := f.metadata[pr].Labels; !containsReviewRequested(got) {
		t.Fatalf("PR labels = %v, want existing review-requested label", got)
	}
}

func TestSubmitReviewDoesNotLabelStackSibling(t *testing.T) {
	f := fixtureRepo(t)
	parent := f.new(t, "EX-1", reviewSubmitTitle, "")
	f.commit(t, "parent-review.txt")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: parent, title: reviewSubmitTitle, bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	parentPR := f.prs[parent]
	child := f.new(t, "EX-2", reviewSubmitTitle, parent)
	f.commit(t, "child-review.txt")

	code, output := runReviewSubmit(t, f, child, true, false)
	childPR := f.prs[child]
	if code != 0 || childPR == 0 || len(f.labelPRs) != 1 || f.labelPRs[0] != childPR || slices.Contains(f.labelPRs, parentPR) {
		t.Fatalf("exit=%d label PRs=%v parent=%d child=%d output=%s", code, f.labelPRs, parentPR, childPR, output)
	}
}

func TestSubmitReviewRetargetAddsLabelAndKeepsRetargetReview(t *testing.T) {
	f := trackingFixture(t, "main", false)
	bookmark := f.new(t, "EX-1", reviewSubmitTitle, "")
	f.commit(t, "initial-retarget.txt")
	opt := submitOptions{bookmark: bookmark, title: reviewSubmitTitle, bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	pr := f.prs[bookmark]
	metadata := f.metadata[pr]
	metadata.BaseRef = f.repo.profile.Mirror
	metadata.BaseSHA = jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(f.repo.profile.Mirror+"@origin"), "-T", "commit_id")
	f.metadata[pr] = metadata
	f.commit(t, "retarget-review.txt")

	code, output := runRetargetReviewSubmit(t, f, bookmark)
	metadata = f.metadata[pr]
	if code != 0 || metadata.BaseRef != f.repo.profile.Trunk || metadata.HeadRef != bookmark || f.labelPosts != 1 || !containsReviewRequested(metadata.Labels) {
		t.Fatalf("exit=%d PR=%+v label posts=%d output=%s", code, metadata, f.labelPosts, output)
	}
	if len(f.labelPRs) != 1 || f.labelPRs[0] != pr || reviewRequestCount(f.reviewCalls) != 1 || len(f.reviewCalls) != 2 || !strings.Contains(f.reviewCalls[0], "/requested_reviewers ") || !strings.Contains(f.reviewCalls[0], f.repo.profile.Reviewer) || !strings.Contains(f.reviewCalls[1], "/comments ") || !strings.Contains(f.reviewCalls[1], "please re-review") || !strings.Contains(f.reviewCalls[1], f.repo.profile.Trunk) {
		t.Fatalf("label PRs=%v retarget review calls=%v", f.labelPRs, f.reviewCalls)
	}
}

func TestSubmitReviewPushFailureSkipsLabel(t *testing.T) {
	f, bookmark := reviewSubmitFixture(t)
	f.pushError = errors.New("fixture push failure")
	code, output := runReviewSubmit(t, f, bookmark, true, false)
	if code == 0 || f.labelPosts != 0 || len(f.pushes) != 1 {
		t.Fatalf("exit=%d label posts=%d pushes=%v output=%s", code, f.labelPosts, f.pushes, output)
	}
}

func TestSubmitReviewReconcileFailureSkipsLabel(t *testing.T) {
	f, bookmark := reviewSubmitFixture(t)
	opt := submitOptions{bookmark: bookmark, title: reviewSubmitTitle, bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	pr := f.prs[bookmark]
	metadata := f.metadata[pr]
	metadata.Title = "remote title"
	f.metadata[pr] = metadata
	f.commit(t, "reconcile-review.txt")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if len(f.pushes) > 1 && req.Method == http.MethodPatch && strings.HasSuffix(req.URL.Path, "/pulls/1") {
			w.WriteHeader(http.StatusInternalServerError)
			respondJSON(t, w, map[string]string{"message": "fixture reconcile failure"})
			return
		}
		f.serve(t, w, req)
	}))
	defer server.Close()
	installFixtureForge(t, f, server)

	code, output := runReviewSubmit(t, f, bookmark, true, false)
	if code == 0 || f.labelPosts != 0 || !strings.Contains(output, "write PR metadata") || !strings.Contains(output, "fixture reconcile failure") {
		t.Fatalf("exit=%d label posts=%d output=%s", code, f.labelPosts, output)
	}
}

func TestSubmitReviewLabelFailureKeepsMovedErrorVisible(t *testing.T) {
	f, bookmark := reviewSubmitFixture(t)
	opt := submitOptions{bookmark: bookmark, title: reviewSubmitTitle, bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.timeline = []map[string]any{{"event": "reviewed", "state": "approved", "id": 1}}
	f.reviewStatus = http.StatusInternalServerError
	f.labelStatus = http.StatusInternalServerError
	f.commit(t, "moved-review.txt")

	code, output := runReviewSubmit(t, f, bookmark, true, false)
	if code != 3 || f.labelPosts != 1 || !strings.Contains(output, "re-request review after moving covered head") || !strings.Contains(output, "fixture review failure") || !strings.Contains(output, "fixture label failure") {
		t.Fatalf("exit=%d label posts=%d output=%s", code, f.labelPosts, output)
	}
}

func TestSubmitReviewLabelFailureReportsRecovery(t *testing.T) {
	f, bookmark := reviewSubmitFixture(t)
	f.labelStatus = 500
	code, output := runReviewSubmit(t, f, bookmark, true, false)
	pr := f.prs[bookmark]
	if code != 3 || f.labelPosts != 1 || pr == 0 {
		t.Fatalf("exit=%d label posts=%d PR=%d output=%s", code, f.labelPosts, pr, output)
	}
	for _, want := range []string{"PR 1", "review was not requested", "rerun rjc submit --review"} {
		if !strings.Contains(output, want) {
			t.Errorf("output %q does not contain %q", output, want)
		}
	}
	if len(f.pushes) != 1 {
		t.Fatalf("pushes = %v, want successful push retained", f.pushes)
	}
}

func TestSubmitReviewDryRunMakesNoForgeWrite(t *testing.T) {
	f, bookmark := reviewSubmitFixture(t)
	code, output := runReviewSubmit(t, f, bookmark, true, true)
	if code != 0 || f.labelPosts != 0 || len(f.pushes) != 0 || len(f.prs) != 0 {
		t.Fatalf("exit=%d label posts=%d pushes=%v PRs=%v output=%s", code, f.labelPosts, f.pushes, f.prs, output)
	}
}

func TestSubmitUpstreamSyncAddsUpstreamSyncLabel(t *testing.T) {
	f, bookmark, opt := upstreamSyncSubmitFixture(t)
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	pr := f.prs[bookmark]
	if f.labelPosts != 1 || len(f.labelPayloads) != 1 || !slices.Equal(f.labelPayloads[0], []string{"upstream-sync"}) || !slices.Equal(f.labelPRs, []int{pr}) {
		t.Fatalf("label posts=%d payloads=%v PRs=%v, want one upstream-sync POST to PR %d", f.labelPosts, f.labelPayloads, f.labelPRs, pr)
	}
}

func TestSubmitUpstreamSyncSkipsExistingLabel(t *testing.T) {
	f, bookmark, opt := upstreamSyncSubmitFixture(t)
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	pr := f.prs[bookmark]
	metadata := f.metadata[pr]
	metadata.Labels = []string{"upstream-sync"}
	f.metadata[pr] = metadata
	f.labelPosts, f.labelPayloads = 0, nil

	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.labelPosts != 0 || !slices.Contains(f.metadata[pr].Labels, "upstream-sync") {
		t.Fatalf("label posts=%d labels=%v, want existing label kept with no POST", f.labelPosts, f.metadata[pr].Labels)
	}
}

func TestSubmitNonSyncTrackingBookmarkAddsNoUpstreamSyncLabel(t *testing.T) {
	f := trackingFixture(t, "main", false)
	bookmark := f.new(t, "EX-1", reviewSubmitTitle, "")
	f.commit(t, "plain-tracking.txt")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: reviewSubmitTitle, bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.labelPosts != 0 || len(f.labelPayloads) != 0 {
		t.Fatalf("label posts=%d payloads=%v, want none for a non-sync bookmark", f.labelPosts, f.labelPayloads)
	}
}

func TestSubmitUpstreamSyncLabelFailureIsLoud(t *testing.T) {
	f, _, opt := upstreamSyncSubmitFixture(t)
	f.labelStatus = http.StatusInternalServerError
	err := f.repo.submit(f.ctx, opt, f, io.Discard)
	detail, ok := errors.AsType[*failure](err)
	if !ok || detail.stage != "submit" || detail.code != 3 || !strings.Contains(detail.reason, "upstream-sync") || !strings.Contains(detail.fix, "rerun rjc submit") || len(f.pushes) != 1 {
		t.Fatalf("err=%v pushes=%v, want loud submit failure with a fix line", err, f.pushes)
	}
}

func TestSubmitUpstreamRecordReadFailureNamesPushedPR(t *testing.T) {
	f, bookmark, opt := upstreamSyncSubmitFixture(t)
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	pr := f.prs[bookmark]
	path, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).path(bookmark, ".upstream.json")
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the record only after the push, so only the post-push read fails.
	f.afterPush = func() {
		if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	err = f.repo.submit(f.ctx, opt, f, io.Discard)
	detail, ok := errors.AsType[*failure](err)
	if !ok || detail.code != 3 || !strings.Contains(detail.reason, fmt.Sprintf("PR %d", pr)) || !strings.Contains(detail.fix, "rerun rjc submit") {
		t.Fatalf("err=%v, want a failure naming PR %d with a rerun fix", err, pr)
	}
}

func TestSubmitDesignOnlyPRAddsDesignLabel(t *testing.T) {
	f := fixtureRepo(t)
	bookmark := f.new(t, "EX-1", reviewSubmitTitle, "")
	f.commit(t, "docs/designs/record.md")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: reviewSubmitTitle, bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	pr := f.prs[bookmark]
	if f.labelPosts != 1 || len(f.labelPayloads) != 1 || !slices.Equal(f.labelPayloads[0], []string{"design"}) || !slices.Equal(f.labelPRs, []int{pr}) {
		t.Fatalf("label posts=%d payloads=%v PRs=%v, want one design POST to PR %d", f.labelPosts, f.labelPayloads, f.labelPRs, pr)
	}
}

func TestSubmitMixedDesignPRAddsNoDesignLabel(t *testing.T) {
	f := fixtureRepo(t)
	bookmark := f.new(t, "EX-1", reviewSubmitTitle, "")
	f.commit(t, "docs/designs/record.md")
	f.commit(t, "tools/y.go")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: reviewSubmitTitle, bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.labelPosts != 0 || len(f.labelPayloads) != 0 {
		t.Fatalf("label posts=%d payloads=%v, want none for a PR that also changes code", f.labelPosts, f.labelPayloads)
	}
}

func TestSubmitDesignLabelSkipsExistingLabel(t *testing.T) {
	f := fixtureRepo(t)
	bookmark := f.new(t, "EX-1", reviewSubmitTitle, "")
	f.commit(t, "docs/designs/record.md")
	opt := submitOptions{bookmark: bookmark, title: reviewSubmitTitle, bodyFile: f.bodyFile}
	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	pr := f.prs[bookmark]
	metadata := f.metadata[pr]
	metadata.Labels = []string{"design"}
	f.metadata[pr] = metadata
	f.labelPosts, f.labelPayloads, f.labelPRs = 0, nil, nil

	if err := f.repo.submit(f.ctx, opt, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.labelPosts != 0 || !slices.Contains(f.metadata[pr].Labels, "design") {
		t.Fatalf("label posts=%d labels=%v, want existing label kept with no POST", f.labelPosts, f.metadata[pr].Labels)
	}
}

func TestSubmitDesignRenameFromCodeAddsNoDesignLabel(t *testing.T) {
	f := fixtureRepo(t)
	writeTestFile(t, filepath.Join(f.repo.root, "tools/y.go"), "package tools\n")
	jjTest(t, f.repo, "commit", "-m", "chore: add tool file")
	jjTest(t, f.repo, "bookmark", "move", "main", "--to", "@-")
	jjTest(t, f.repo, "git", "push", "--bookmark", "main")
	jjTest(t, f.repo, "new", "main")
	bookmark := f.new(t, "EX-1", reviewSubmitTitle, "")
	if err := os.MkdirAll(filepath.Join(f.repo.root, "docs/designs"), 0o700); err != nil {
		t.Fatal(err)
	}
	// jj --name-only lists only the new path of a rename; the removed tools/y.go must still count.
	if err := os.Rename(filepath.Join(f.repo.root, "tools/y.go"), filepath.Join(f.repo.root, "docs/designs/y.go")); err != nil {
		t.Fatal(err)
	}
	jjTest(t, f.repo, "commit", "-m", "docs(designs): move tool file into designs")
	if err := f.repo.submit(f.ctx, submitOptions{bookmark: bookmark, title: reviewSubmitTitle, bodyFile: f.bodyFile}, f, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.labelPosts != 0 || len(f.labelPayloads) != 0 {
		t.Fatalf("label posts=%d payloads=%v, want none when a code file moves into docs/designs", f.labelPosts, f.labelPayloads)
	}
}

func designStackFixture(t *testing.T, f *fixture, lowerPath, upperPath string) (string, string) {
	t.Helper()
	lower := f.new(t, "EX-1", reviewSubmitTitle, "")
	f.commit(t, lowerPath)
	jjTest(t, f.repo, "bookmark", "move", lower, "--to", "@-")
	var out bytes.Buffer
	if code := run(f.ctx, []string{"meta", lower, "--title", reviewSubmitTitle, "--body-file", f.bodyFile}, &out, &out); code != 0 {
		t.Fatalf("meta exit=%d: %s", code, &out)
	}
	upper, err := f.repo.newChange(f.ctx, newOptions{issue: "EX-2", title: reviewSubmitTitle, on: lower})
	if err != nil {
		t.Fatal(err)
	}
	f.commit(t, upperPath)
	return lower, upper
}

func TestSubmitDesignLabelFailureKeepsStackPushing(t *testing.T) {
	f := fixtureRepo(t)
	lower, upper := designStackFixture(t, f, "docs/designs/parent.md", "docs/designs/child.md")
	f.labelStatus = http.StatusInternalServerError

	code, output := runReviewSubmit(t, f, upper, false, false)
	if code != 3 || !slices.Equal(f.pushes, []string{lower, upper}) || f.labelPosts != 2 || !strings.Contains(output, "design label was not added") {
		t.Fatalf("exit=%d pushes=%v label posts=%d output=%s", code, f.pushes, f.labelPosts, output)
	}
}

func TestSubmitDesignLabelOnlyOnDesignOnlyUpperPR(t *testing.T) {
	f := fixtureRepo(t)
	lower, upper := designStackFixture(t, f, "tools/parent.go", "docs/designs/child.md")

	code, output := runReviewSubmit(t, f, upper, false, false)
	upperPR := f.prs[upper]
	if code != 0 || upperPR == 0 || !slices.Equal(f.labelPRs, []int{upperPR}) || f.prs[lower] == upperPR {
		t.Fatalf("exit=%d label PRs=%v upper PR=%d output=%s", code, f.labelPRs, upperPR, output)
	}
}

func TestSubmitDesignLabelNeedsRepoOptIn(t *testing.T) {
	f := fixtureRepo(t)
	bookmark := f.new(t, "EX-1", reviewSubmitTitle, "")
	f.commit(t, "docs/designs/record.md")
	f.repo.profile.DesignLabel = false

	code, output := runReviewSubmit(t, f, bookmark, false, false)
	if code != 0 || f.labelPosts != 0 {
		t.Fatalf("exit=%d label posts=%d payloads=%v output=%s", code, f.labelPosts, f.labelPayloads, output)
	}
}

func upstreamSyncSubmitFixture(t *testing.T) (*fixture, string, submitOptions) {
	t.Helper()
	f := trackingFixture(t, "main", false)
	bookmark, err := f.repo.upstreamSync(f.ctx, upstreamSyncOptions{issue: "EX-1"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok, err := (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).LoadUpstream(bookmark)
	if err != nil || !ok {
		t.Fatalf("record: %v %v", ok, err)
	}
	return f, bookmark, submitOptions{bookmark: bookmark, title: "chore(fork): merge upstream main " + rec.Upstream[:12], bodyFile: f.bodyFile}
}

func reviewSubmitFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := fixtureRepo(t)
	bookmark := f.new(t, "EX-1", reviewSubmitTitle, "")
	f.commit(t, "review-round.txt")
	return f, bookmark
}

func runReviewSubmit(t *testing.T, f *fixture, bookmark string, review, dryRun bool) (int, string) {
	t.Helper()
	args := []string{"submit", bookmark, "--title", reviewSubmitTitle, "--body-file", f.bodyFile}
	if review {
		args = append(args, "--review")
	}
	if dryRun {
		args = append(args, "--dry-run")
	}
	var output bytes.Buffer
	code := run(f.ctx, args, &output, &output)
	return code, output.String()
}

func runRetargetReviewSubmit(t *testing.T, f *fixture, bookmark string) (int, string) {
	t.Helper()
	args := []string{"submit", bookmark, "--title", reviewSubmitTitle, "--body-file", f.bodyFile, "--retarget", "--review"}
	var output bytes.Buffer
	code := run(f.ctx, args, &output, &output)
	return code, output.String()
}

func installFixtureForge(t *testing.T, f *fixture, server *httptest.Server) {
	t.Helper()
	s, err := service(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.forge = &forgeClient{baseURL: server.URL, client: server.Client()}
}

func containsReviewRequested(labels []string) bool {
	return slices.Contains(labels, "review-requested")
}
