package cdgate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/cdrelease"
	"github.com/RigelBuild/nebula/tools/go-lib/woodpecker"
)

var fenceNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func fenceClock() time.Time { return fenceNow }

// memQueue is an in-memory cdrelease.Store. Update applies fn and keeps the
// result, as the ConfigMap store does on a successful compare-and-swap. An error
// from fn is a refusal, so nothing is written.
type memQueue struct {
	state   cdrelease.State
	updates int
	writes  int
}

func (q *memQueue) Update(_ context.Context, fn func(cdrelease.State) (cdrelease.State, error)) (cdrelease.State, error) {
	q.updates++
	next, err := fn(q.state)
	if err != nil {
		return cdrelease.State{}, err
	}
	q.writes++
	q.state = next
	return next, nil
}

// queueRunning returns a queue whose release is pipeline with dispatchID and no
// heartbeat yet.
func queueRunning(pipeline int64, dispatchID string) *memQueue {
	return &memQueue{state: cdrelease.State{Release: &cdrelease.Release{
		Pipeline:   pipeline,
		DispatchID: dispatchID,
		Commit:     "c",
		ClaimedAt:  fenceNow.Add(-time.Minute).Format(time.RFC3339),
	}}}
}

// countingExec returns an executor that counts deploy runs and succeeds.
func countingExec(runs *int) Executor {
	return func(context.Context, bool) (StepResult, error) {
		*runs++
		return StepResult{Code: 0}, nil
	}
}

func TestReleaseFenceStaleDispatchIDWritesNothing(t *testing.T) {
	q := queueRunning(7, "current")
	ok, err := ReleaseFence(q, 7, "stale", fenceClock)(t.Context())
	if err != nil {
		t.Fatalf("fence: %v", err)
	}
	if ok {
		t.Fatal("fence proceeded for a stale dispatchId; want false")
	}
	if q.writes != 0 || q.state.Release.Heartbeat != "" {
		t.Fatalf("a fenced-out step wrote the queue: writes=%d release=%+v", q.writes, q.state.Release)
	}
}

func TestReleaseFenceCurrentStepHeartbeatsOnce(t *testing.T) {
	q := queueRunning(7, "current")
	ok, err := ReleaseFence(q, 7, "current", fenceClock)(t.Context())
	if err != nil {
		t.Fatalf("fence: %v", err)
	}
	if !ok {
		t.Fatal("fence refused the current release")
	}
	if q.updates != 1 || q.writes != 1 {
		t.Fatalf("queue updates=%d writes=%d; want one CAS heartbeat", q.updates, q.writes)
	}
	if got, want := q.state.Release.Heartbeat, fenceNow.Format(time.RFC3339); got != want {
		t.Fatalf("heartbeat = %q, want %q", got, want)
	}
}

// A takeover moved the release to pipeline 9. The stalled pipeline 7 must
// neither write nor proceed.
func TestReleaseFenceTakenOverPipelineWritesNothing(t *testing.T) {
	q := queueRunning(9, "d9")
	ok, err := ReleaseFence(q, 7, "d7", fenceClock)(t.Context())
	if err != nil {
		t.Fatalf("fence: %v", err)
	}
	if ok || q.writes != 0 {
		t.Fatalf("stalled pipeline proceeded=%v writes=%d; want fenced out with no write", ok, q.writes)
	}
}

func TestRunCDStaleDispatchIDExitsZeroWithoutDeploy(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1"}}
	q := queueRunning(7, "current")
	deploys := 0
	r, _ := runFixture(t, Verdict{Deploy: true}, countingExec(&deploys))
	r.Lease = lease(api)
	r.Fence = ReleaseFence(q, 7, "stale", fenceClock)
	var skipped Verdict
	r.Skip = func(v Verdict) { skipped = v }
	if code := RunCD(t.Context(), "nix:deploy", r); code != 0 {
		t.Fatalf("RunCD = %d, want 0 for a stale dispatchId", code)
	}
	if deploys != 0 {
		t.Fatalf("deploy ran %d times for a stale dispatchId", deploys)
	}
	if last := api.writes[len(api.writes)-1]; last.HolderIdentity != "" {
		t.Fatalf("final Lease write = %+v, want the Lease released", last)
	}
	if skipped.Reason != ReasonNotCurrentRelease {
		t.Fatalf("skip reason = %q, want %q", skipped.Reason, ReasonNotCurrentRelease)
	}
}

func TestRunCDCurrentStepHeartbeatsBeforeDeploy(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1"}}
	q := queueRunning(7, "current")
	var order []string
	exec := func(context.Context, bool) (StepResult, error) {
		order = append(order, "deploy")
		return StepResult{Code: 0}, nil
	}
	fence := ReleaseFence(q, 7, "current", fenceClock)
	r, _ := runFixture(t, Verdict{Deploy: true}, exec)
	r.Lease = lease(api)
	r.Fence = func(ctx context.Context) (bool, error) {
		order = append(order, "fence")
		return fence(ctx)
	}
	if code := RunCD(t.Context(), "nix:deploy", r); code != 0 {
		t.Fatalf("RunCD = %d, want 0", code)
	}
	if got := strings.Join(order, ","); got != "fence,deploy" {
		t.Fatalf("order = %s, want the heartbeat before the deploy command", got)
	}
	if q.writes != 1 {
		t.Fatalf("queue writes = %d, want one heartbeat", q.writes)
	}
}

func TestRunCDFenceErrorExitsOneWithoutDeploy(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1"}}
	deploys := 0
	r, _ := runFixture(t, Verdict{Deploy: true}, countingExec(&deploys))
	r.Lease = lease(api)
	r.Fence = func(context.Context) (bool, error) { return false, errors.New("queue unreachable") }
	skipped := 0
	r.Skip = func(Verdict) { skipped++ }
	if code := RunCD(t.Context(), "nix:deploy", r); code != 1 {
		t.Fatalf("RunCD = %d, want 1 when the fence cannot read the queue", code)
	}
	if deploys != 0 {
		t.Fatalf("deploy ran %d times after a fence error", deploys)
	}
	if skipped != 0 {
		t.Fatalf("fence error recorded %d skip(s); a failed fence must not read as not-current-release", skipped)
	}
}

// Two later steps of a stalled pipeline both run after the takeover. Neither
// deploys.
func TestRunCDTakenOverStepsDeployNothing(t *testing.T) {
	q := queueRunning(9, "d9")
	deploys := 0
	for range 2 {
		api := &memLease{record: &LeaseRecord{ResourceVersion: "1"}}
		r, _ := runFixture(t, Verdict{Deploy: true}, countingExec(&deploys))
		r.Lease = lease(api)
		r.Fence = ReleaseFence(q, 7, "d7", fenceClock)
		if code := RunCD(t.Context(), "nix:deploy", r); code != 0 {
			t.Fatalf("RunCD = %d, want 0 for a fenced-out step", code)
		}
	}
	if deploys != 0 || q.writes != 0 {
		t.Fatalf("deploys=%d queue writes=%d after takeover; want none", deploys, q.writes)
	}
}

func TestRunCDFenceWithoutLeaseFailsClosed(t *testing.T) {
	q := queueRunning(7, "current")
	deploys := 0
	r, _ := runFixture(t, Verdict{Deploy: true}, countingExec(&deploys))
	r.Fence = ReleaseFence(q, 7, "current", fenceClock)
	if code := RunCD(t.Context(), "nix:deploy", r); code != 1 {
		t.Fatalf("RunCD = %d, want 1: a release fence cannot be checked without the Lease", code)
	}
	if deploys != 0 || q.updates != 0 {
		t.Fatalf("deploys=%d queue updates=%d; want none", deploys, q.updates)
	}
}

func TestRunCDPublishFenceNotNewestExits79WithoutGate(t *testing.T) {
	deploys := 0
	r, log := runFixture(t, Verdict{Deploy: true}, countingExec(&deploys))
	r.Gate = func(context.Context) (Verdict, error) {
		t.Error("the gate ran before the publish fence")
		return Verdict{Deploy: true}, nil
	}
	var skipped Verdict
	r.Skip = func(v Verdict) { skipped = v }
	r.PublishFence = func(context.Context) (bool, error) { return false, nil }
	if code := RunCD(t.Context(), "nix:deploy", r); code != ReleaseFenceExitCode {
		t.Fatalf("RunCD = %d, want %d for a publish that is not the newest release", code, ReleaseFenceExitCode)
	}
	if deploys != 0 {
		t.Fatalf("publish ran %d times although it is not the newest release", deploys)
	}
	if skipped.Reason != ReasonNotNewestRelease {
		t.Fatalf("skip reason = %q, want %q", skipped.Reason, ReasonNotNewestRelease)
	}
	if !strings.Contains(log.String(), "not the newest release") {
		t.Fatalf("log = %q, want the not-the-newest-release line", log.String())
	}
}

func TestRunCDPublishFenceNewestDeploys(t *testing.T) {
	deploys := 0
	r, _ := runFixture(t, Verdict{Deploy: true}, countingExec(&deploys))
	r.PublishFence = func(context.Context) (bool, error) { return true, nil }
	if code := RunCD(t.Context(), "nix:deploy", r); code != 0 {
		t.Fatalf("RunCD = %d, want 0", code)
	}
	if deploys != 1 {
		t.Fatalf("publish runs = %d, want 1", deploys)
	}
}

func TestRunCDPublishFenceErrorExitsOneWithoutDeploy(t *testing.T) {
	deploys := 0
	r, _ := runFixture(t, Verdict{Deploy: true}, countingExec(&deploys))
	r.PublishFence = func(context.Context) (bool, error) { return false, errors.New("api down") }
	if code := RunCD(t.Context(), "nix:deploy", r); code != 1 {
		t.Fatalf("RunCD = %d, want 1 when the publish fence cannot read the pipelines", code)
	}
	if deploys != 0 {
		t.Fatalf("publish ran %d times after a fence error", deploys)
	}
}

// manualMain is one manual main pipeline row. A non-empty dispatchID is the
// PETREL_CD_DISPATCH_ID variable a release carries.
func manualMain(number int64, dispatchID string) map[string]any {
	item := map[string]any{"number": number, "commit": "c", "branch": "main", "event": "manual"}
	if dispatchID != "" {
		item["variables"] = map[string]string{DispatchIDVariable: dispatchID}
	}
	return item
}

// releaseList serves items as the manual main pipelines on page 1, and an empty
// page after it.
func releaseList(t *testing.T, items []map[string]any) PipelineAPI {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/api/repos/7/pipelines" || q.Get("event") != "manual" || q.Get("branch") != "main" {
			http.NotFound(w, r)
			return
		}
		page := []map[string]any{}
		if q.Get("page") == "1" {
			page = items
		}
		if err := json.NewEncoder(w).Encode(page); err != nil {
			t.Errorf("encode pipelines: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return PipelineAPI{Client: &woodpecker.Client{BaseURL: srv.URL, Token: "t"}, RepoID: 7}
}

func TestIsNewestReleaseOnlyForTheNewestDispatchedPipeline(t *testing.T) {
	cases := map[string]struct {
		items []map[string]any
		own   int64
		want  bool
	}{
		"own is the newest release":                         {[]map[string]any{manualMain(10, "d10"), manualMain(9, "d9")}, 10, true},
		"a newer release exists":                            {[]map[string]any{manualMain(11, "d11"), manualMain(10, "d10")}, 10, false},
		"a newer manual pipeline without a dispatch is not": {[]map[string]any{manualMain(12, ""), manualMain(10, "d10")}, 10, true},
		"own carries no dispatch id":                        {[]map[string]any{manualMain(10, ""), manualMain(9, "d9")}, 10, false},
		"own is not in the list":                            {[]map[string]any{manualMain(9, "d9")}, 10, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := releaseList(t, tc.items).IsNewestRelease(t.Context(), tc.own)
			if err != nil {
				t.Fatalf("IsNewestRelease: %v", err)
			}
			if got != tc.want {
				t.Fatalf("IsNewestRelease = %v, want %v", got, tc.want)
			}
		})
	}
}

// pagedReleaseList serves items newest first in pages of pageSize, as
// Woodpecker does, and counts the pages read.
func pagedReleaseList(t *testing.T, items []map[string]any) (PipelineAPI, *int) {
	t.Helper()
	reads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/api/repos/7/pipelines" || q.Get("event") != "manual" || q.Get("branch") != "main" || q.Get("perPage") != strconv.Itoa(pageSize) {
			http.NotFound(w, r)
			return
		}
		reads++
		page, err := strconv.Atoi(q.Get("page"))
		if err != nil || page < 1 {
			http.NotFound(w, r)
			return
		}
		start := min((page-1)*pageSize, len(items))
		end := min(start+pageSize, len(items))
		if err := json.NewEncoder(w).Encode(items[start:end]); err != nil {
			t.Errorf("encode pipelines: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return PipelineAPI{Client: &woodpecker.Client{BaseURL: srv.URL, Token: "t"}, RepoID: 7}, &reads
}

// plainManuals returns n manual main pipelines without a dispatch id, numbered
// down from top.
func plainManuals(top int64, n int) []map[string]any {
	out := make([]map[string]any, 0, n)
	for i := range n {
		out = append(out, manualMain(top-int64(i), ""))
	}
	return out
}

func TestIsNewestReleaseReadsPastTheFirstPage(t *testing.T) {
	cases := map[string]struct {
		items     []map[string]any
		want      bool
		wantReads int
	}{
		"own release on page 2 behind plain manuals": {
			append(plainManuals(200, pageSize), manualMain(100, "d100")), true, 2,
		},
		"a newer release on page 2 behind plain manuals": {
			append(plainManuals(200, pageSize), manualMain(150, "d150"), manualMain(100, "d100")), false, 2,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api, reads := pagedReleaseList(t, tc.items)
			got, err := api.IsNewestRelease(t.Context(), 100)
			if err != nil {
				t.Fatalf("IsNewestRelease: %v", err)
			}
			if got != tc.want || *reads != tc.wantReads {
				t.Fatalf("IsNewestRelease = %v after %d page reads, want %v after %d", got, *reads, tc.want, tc.wantReads)
			}
		})
	}
}

// Own is absent and the scan reaches a pipeline numbered below own: it stops
// there, without reading the next page.
func TestIsNewestReleaseStopsAtOwnNumber(t *testing.T) {
	items := append(plainManuals(149, pageSize-1), manualMain(90, ""))
	items = append(items, plainManuals(89, pageSize)...)
	items = append(items, manualMain(10, "d10"))
	api, reads := pagedReleaseList(t, items)
	got, err := api.IsNewestRelease(t.Context(), 100)
	if err != nil {
		t.Fatalf("IsNewestRelease: %v", err)
	}
	if got || *reads != 1 {
		t.Fatalf("IsNewestRelease = %v after %d page reads, want false after 1", got, *reads)
	}
}

func TestParsePipelineItemKeepsVariables(t *testing.T) {
	item, err := parsePipelineItem(map[string]any{
		"number": 4.0, "commit": "c", "branch": "main", "event": "manual",
		"variables": map[string]any{DispatchIDVariable: "d4"},
	})
	if err != nil {
		t.Fatalf("parsePipelineItem: %v", err)
	}
	if item.Variables[DispatchIDVariable] != "d4" {
		t.Fatalf("variables = %v, want the dispatch id kept", item.Variables)
	}
}

func TestParsePipelineItemRejectsNonStringVariables(t *testing.T) {
	_, err := parsePipelineItem(map[string]any{
		"number": 4.0, "commit": "c", "branch": "main", "event": "manual",
		"variables": map[string]any{"X": 1.0},
	})
	if err == nil {
		t.Fatal("parsePipelineItem accepted a non-string variable")
	}
}
