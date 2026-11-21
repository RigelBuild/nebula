package woodpecker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// testDispatchID is the caller-minted dispatch id the tests pass in. The
// dispatch must send it verbatim as PETREL_CD_DISPATCH_ID.
const testDispatchID = "11111111-2222-4333-8444-555555555555"

// dispatchNonce matches the v4 UUID a caller mints with NewDispatchID.
var dispatchNonce = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// dispatchStart is the fake clock's time when a dispatch begins.
var dispatchStart = time.Unix(1_800_000_000, 0)

// fakeClock is the dispatch clock under test. sleep advances now, so the
// 180-second confirmation window passes without real waiting; out captures
// the confirmation log.
type fakeClock struct {
	now    time.Time
	sleeps []time.Duration
	out    bytes.Buffer
}

func (f *fakeClock) seam() dispatchClock {
	return dispatchClock{
		now: func() time.Time { return f.now },
		sleep: func(_ context.Context, d time.Duration) error {
			f.sleeps = append(f.sleeps, d)
			f.now = f.now.Add(d)
			return nil
		},
		log: &f.out,
	}
}

// fakeWoodpecker answers each create POST with replies in order (the last one
// repeats), and answers the pipeline list GET with list(nonce), where nonce is
// the PETREL_CD_DISPATCH_ID the most recent create carried.
type fakeWoodpecker struct {
	tb         testing.TB
	replies    []int
	list       func(nonce string) []map[string]any
	listStatus int // when non-zero, the list GET answers with this status

	mu    sync.Mutex
	posts int
	gets  int
	nonce string
}

type fakeCounts struct {
	posts int
	gets  int
	nonce string
}

func (f *fakeWoodpecker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodPost {
		status := f.replies[len(f.replies)-1]
		if f.posts < len(f.replies) {
			status = f.replies[f.posts]
		}
		f.posts++
		var body struct {
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.tb.Errorf("decode create body: %v", err)
		}
		f.nonce = body.Variables["PETREL_CD_DISPATCH_ID"]
		if status == http.StatusNoContent {
			w.WriteHeader(status)
			return
		}
		http.Error(w, http.StatusText(status), status)
		return
	}
	f.gets++
	if f.listStatus != 0 {
		http.Error(w, http.StatusText(f.listStatus), f.listStatus)
		return
	}
	if err := json.NewEncoder(w).Encode(f.list(f.nonce)); err != nil {
		f.tb.Errorf("encode list: %v", err)
	}
}

func (f *fakeWoodpecker) counts() fakeCounts {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fakeCounts{posts: f.posts, gets: f.gets, nonce: f.nonce}
}

func dispatchEntry(number int, created int64, nonce string) map[string]any {
	return map[string]any{
		"number":    number,
		"created":   created,
		"branch":    "main",
		"event":     "manual",
		"variables": map[string]any{"PETREL_CD_DISPATCH_ID": nonce},
	}
}

func newFakeWoodpecker(t *testing.T, fw *fakeWoodpecker) Context {
	t.Helper()
	fw.tb = t
	srv := httptest.NewServer(fw)
	t.Cleanup(srv.Close)
	return Context{BaseURL: srv.URL, Token: "token", RepoID: 1}
}

func emptyList(string) []map[string]any { return []map[string]any{} }

func TestDispatchCDTargetsConfirms5xxWithoutRePosting(t *testing.T) {
	fw := &fakeWoodpecker{
		replies: []int{http.StatusBadGateway},
		list: func(nonce string) []map[string]any {
			return []map[string]any{dispatchEntry(23769, dispatchStart.Unix(), nonce)}
		},
	}
	ctx := newFakeWoodpecker(t, fw)
	clock := &fakeClock{now: dispatchStart}

	number, err := dispatchCDTargets(context.Background(), ctx, []string{"first"}, testDispatchID, "", 0, clock.seam())
	if err != nil {
		t.Fatalf("dispatchCDTargets: %v", err)
	}
	if number != 23769 {
		t.Fatalf("number = %d, want 23769", number)
	}
	got := fw.counts()
	if got.posts != 1 || got.gets < 1 {
		t.Fatalf("posts = %d, gets = %d; want one POST and at least one list GET", got.posts, got.gets)
	}
	if got.nonce != testDispatchID {
		t.Fatalf("PETREL_CD_DISPATCH_ID = %q, want the caller's %q", got.nonce, testDispatchID)
	}
	assertConfirmedLog(t, &clock.out, 23769, http.StatusBadGateway)
}

func TestDispatchCDTargetsRetriesOnceWhenNoPipelineConfirms(t *testing.T) {
	fw := &fakeWoodpecker{
		replies: []int{http.StatusBadGateway, http.StatusNoContent},
		list:    emptyList,
	}
	ctx := newFakeWoodpecker(t, fw)
	clock := &fakeClock{now: dispatchStart}

	number, err := dispatchCDTargets(context.Background(), ctx, []string{"first"}, testDispatchID, "", 0, clock.seam())
	if err != nil {
		t.Fatalf("dispatchCDTargets: %v", err)
	}
	if number != 0 {
		t.Fatalf("number = %d, want 0 for a 204 reply", number)
	}
	if got := fw.counts().posts; got != 2 {
		t.Fatalf("posts = %d, want 2 (initial dispatch and one retry)", got)
	}
	if len(clock.sleeps) == 0 || clock.sleeps[len(clock.sleeps)-1] != time.Second {
		t.Fatalf("sleeps = %v, want the last sleep to be 1s before the retry", clock.sleeps)
	}
}

// DispatchWorstCase is the longest the double-5xx path can take: the time the
// fake clock sleeps through both confirm windows and the retry pause, plus two
// POSTs at their dispatchTimeout and two final list requests at GateTimeout,
// which can start at a confirm deadline and overrun it.
func TestDispatchWorstCaseMatchesDoubleFailurePath(t *testing.T) {
	fw := &fakeWoodpecker{
		replies: []int{http.StatusBadGateway, http.StatusBadGateway},
		list:    emptyList,
	}
	ctx := newFakeWoodpecker(t, fw)
	clock := &fakeClock{now: dispatchStart}

	if _, err := dispatchCDTargets(t.Context(), ctx, []string{"first"}, testDispatchID, "", 0, clock.seam()); err == nil {
		t.Fatal("dispatchCDTargets returned nil, want the failure after retry")
	}
	if got := fw.counts().posts; got != 2 {
		t.Fatalf("posts = %d, want 2", got)
	}
	var slept time.Duration
	for _, d := range clock.sleeps {
		slept += d
	}
	if got := slept + 2*dispatchTimeout + 2*GateTimeout; got != DispatchWorstCase {
		t.Fatalf("double-5xx path takes up to %v (slept %v), DispatchWorstCase = %v", got, slept, DispatchWorstCase)
	}
}

func TestDispatchCDTargetsDoesNotRetryA4xx(t *testing.T) {
	fw := &fakeWoodpecker{replies: []int{http.StatusBadRequest}, list: emptyList}
	ctx := newFakeWoodpecker(t, fw)
	clock := &fakeClock{now: dispatchStart}

	_, err := dispatchCDTargets(context.Background(), ctx, []string{"first"}, testDispatchID, "", 0, clock.seam())
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Status != http.StatusBadRequest {
		t.Fatalf("err = %v, want StatusError 400", err)
	}
	if got := fw.counts(); got.posts != 1 || got.gets != 0 {
		t.Fatalf("posts = %d, gets = %d; want one POST and no lookup", got.posts, got.gets)
	}
}

func TestDispatchCDTargetsFailsClosedOnFullPageInsideWindow(t *testing.T) {
	fw := &fakeWoodpecker{
		replies: []int{http.StatusBadGateway},
		list: func(string) []map[string]any {
			page := make([]map[string]any, 0, 50)
			for i := range 50 {
				page = append(page, dispatchEntry(100-i, dispatchStart.Unix(), "someone-else"))
			}
			return page
		},
	}
	ctx := newFakeWoodpecker(t, fw)
	clock := &fakeClock{now: dispatchStart}

	_, err := dispatchCDTargets(context.Background(), ctx, []string{"first"}, testDispatchID, "", 0, clock.seam())
	if err == nil || !strings.Contains(err.Error(), "could not confirm the CD dispatch") {
		t.Fatalf("err = %v, want a could-not-confirm failure", err)
	}
	if got := fw.counts().posts; got != 1 {
		t.Fatalf("posts = %d, want 1 (no retry when the lookup is inconclusive)", got)
	}
}

func TestDispatchCDTargetsReportsBothDispatchesFailing(t *testing.T) {
	fw := &fakeWoodpecker{
		replies: []int{http.StatusBadGateway, http.StatusBadGateway},
		list:    emptyList,
	}
	ctx := newFakeWoodpecker(t, fw)
	clock := &fakeClock{now: dispatchStart}

	_, err := dispatchCDTargets(context.Background(), ctx, []string{"first"}, testDispatchID, "", 0, clock.seam())
	if err == nil || !strings.Contains(err.Error(), "dispatch failed after retry") {
		t.Fatalf("err = %v, want a failed-after-retry error", err)
	}
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Status != http.StatusBadGateway {
		t.Fatalf("err = %v, want it to wrap StatusError 502", err)
	}
	if got := fw.counts().posts; got != 2 {
		t.Fatalf("posts = %d, want 2", got)
	}
}
