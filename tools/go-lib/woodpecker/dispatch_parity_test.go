package woodpecker

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// These cover the six TS dispatch cases of the TypeScript sender that
// the first Go port left without a test. Each runs against the real
// dispatchCDTargets through fakeWoodpecker, with the clock faked.

// TS: "confirms a matching nonce despite a pipeline timestamp skew".
// The matching pipeline was created 5 seconds before the dispatch started.
func TestDispatchCDTargetsConfirmsNonceDespiteTimestampSkew(t *testing.T) {
	fw := &fakeWoodpecker{
		replies: []int{http.StatusBadGateway},
		list: func(nonce string) []map[string]any {
			return []map[string]any{dispatchEntry(10, dispatchStart.Unix()-5, nonce)}
		},
	}
	ctx := newFakeWoodpecker(t, fw)
	clock := &fakeClock{now: dispatchStart}

	number, err := dispatchCDTargets(context.Background(), ctx, []string{"first"}, testDispatchID, "", 0, clock.seam())
	if err != nil {
		t.Fatalf("dispatchCDTargets: %v", err)
	}
	if number != 10 {
		t.Fatalf("number = %d, want 10", number)
	}
	if got := fw.counts().posts; got != 1 {
		t.Fatalf("posts = %d, want 1 (a skewed match must not trigger a retry)", got)
	}
}

// TS: "confirms a match that appears on a later poll". No pipeline carries the
// nonce on the first two polls; the third poll finds it.
func TestDispatchCDTargetsConfirmsMatchOnLaterPoll(t *testing.T) {
	fw := &fakeWoodpecker{replies: []int{http.StatusBadGateway}}
	polls := 0
	fw.list = func(nonce string) []map[string]any {
		polls++
		if polls < 3 {
			return []map[string]any{}
		}
		return []map[string]any{dispatchEntry(11, dispatchStart.Unix(), nonce)}
	}
	ctx := newFakeWoodpecker(t, fw)
	clock := &fakeClock{now: dispatchStart}

	number, err := dispatchCDTargets(context.Background(), ctx, []string{"first"}, testDispatchID, "", 0, clock.seam())
	if err != nil {
		t.Fatalf("dispatchCDTargets: %v", err)
	}
	if number != 11 {
		t.Fatalf("number = %d, want 11", number)
	}
	if polls != 3 {
		t.Fatalf("polls = %d, want 3", polls)
	}
	if got := fw.counts().posts; got != 1 {
		t.Fatalf("posts = %d, want 1", got)
	}
	assertConfirmedLog(t, &clock.out, 11, http.StatusBadGateway)
}

// TS: "rejects when pipeline lookup returns a 500". The create answers 502,
// then the lookup answers 500. The error must say the dispatch was not confirmed.
func TestDispatchCDTargetsRejectsWhenLookupReturns500(t *testing.T) {
	fw := &fakeWoodpecker{replies: []int{http.StatusBadGateway}}
	ctx := newFakeWoodpecker(t, fw)
	fw.list = nil
	fw.listStatus = http.StatusInternalServerError
	clock := &fakeClock{now: dispatchStart}

	_, err := dispatchCDTargets(context.Background(), ctx, []string{"first"}, testDispatchID, "", 0, clock.seam())
	if err == nil || !strings.Contains(err.Error(), "could not confirm the CD dispatch") {
		t.Fatalf("err = %v, want a could-not-confirm failure", err)
	}
	if got := fw.counts().posts; got != 1 {
		t.Fatalf("posts = %d, want 1 (no retry when the lookup failed)", got)
	}
}

// TS: "rejects malformed pipeline list items". A list item whose number is not
// a number must fail the confirmation, not be skipped as a non-match.
func TestDispatchCDTargetsRejectsMalformedListItems(t *testing.T) {
	fw := &fakeWoodpecker{
		replies: []int{http.StatusBadGateway},
		list: func(string) []map[string]any {
			return []map[string]any{{"number": "bad"}}
		},
	}
	ctx := newFakeWoodpecker(t, fw)
	clock := &fakeClock{now: dispatchStart}

	_, err := dispatchCDTargets(context.Background(), ctx, []string{"first"}, testDispatchID, "", 0, clock.seam())
	if err == nil || !strings.Contains(err.Error(), "could not confirm the CD dispatch") {
		t.Fatalf("err = %v, want a could-not-confirm failure", err)
	}
	if got := fw.counts().posts; got != 1 {
		t.Fatalf("posts = %d, want 1", got)
	}
}

// TS: "keeps polling when only the newest item on a full page is recent".
// A full page of 50 items, all but the newest older than the window. The
// poll must keep going rather than fail closed, and the retry then runs.
func TestDispatchCDTargetsKeepsPollingWhenOnlyNewestItemIsRecent(t *testing.T) {
	startedAt := dispatchStart.Unix()
	fw := &fakeWoodpecker{replies: []int{http.StatusBadGateway, http.StatusNoContent}}
	fw.list = func(string) []map[string]any {
		page := make([]map[string]any, 0, 50)
		for i := range 50 {
			created := startedAt - 120
			if i == 0 {
				created = startedAt
			}
			page = append(page, dispatchEntry(100-i, created, "someone-else"))
		}
		return page
	}
	ctx := newFakeWoodpecker(t, fw)
	clock := &fakeClock{now: dispatchStart}

	if _, err := dispatchCDTargets(context.Background(), ctx, []string{"first"}, testDispatchID, "", 0, clock.seam()); err != nil {
		t.Fatalf("dispatchCDTargets: %v", err)
	}
	if got := fw.counts().posts; got != 2 {
		t.Fatalf("posts = %d, want 2 (the page was not fail-closed, so the retry ran)", got)
	}
}

// TS: "confirms a pipeline created after the retry 5xx". The first create and
// its retry both answer 502; the lookup after the retry finds the nonce.
func TestDispatchCDTargetsConfirmsAfterRetry5xx(t *testing.T) {
	fw := &fakeWoodpecker{replies: []int{http.StatusBadGateway, http.StatusBadGateway}}
	// Empty until the second create, then the nonce appears in the list.
	fw.list = func(nonce string) []map[string]any {
		if fw.posts < 2 { // list runs under fw.mu from ServeHTTP; counts() would deadlock
			return []map[string]any{}
		}
		return []map[string]any{dispatchEntry(12, dispatchStart.Unix(), nonce)}
	}
	ctx := newFakeWoodpecker(t, fw)
	clock := &fakeClock{now: dispatchStart}

	number, err := dispatchCDTargets(context.Background(), ctx, []string{"first"}, testDispatchID, "", 0, clock.seam())
	if err != nil {
		t.Fatalf("dispatchCDTargets: %v", err)
	}
	if number != 12 {
		t.Fatalf("number = %d, want 12", number)
	}
	if got := fw.counts().posts; got != 2 {
		t.Fatalf("posts = %d, want 2", got)
	}
	assertConfirmedLog(t, &clock.out, 12, http.StatusBadGateway)
}

// assertConfirmedLog checks the one line the TS confirmCdDispatch printed.
func assertConfirmedLog(t *testing.T, out *bytes.Buffer, number, status int) {
	t.Helper()
	want := fmt.Sprintf("cd-dispatch: confirmed pipeline %d after Woodpecker returned %d\n", number, status)
	if got := out.String(); got != want {
		t.Fatalf("log = %q, want %q", got, want)
	}
}

// A list item whose branch, event, or variables has the wrong type must fail
// the confirmation, as the TS findCreatedCdPipeline did. Skipping it as a
// non-match would let the retry POST a second dispatch.
func TestDispatchCDTargetsRejectsMalformedListItemShapes(t *testing.T) {
	cases := map[string]func(item map[string]any){
		"branch not a string":        func(item map[string]any) { item["branch"] = 1 },
		"event not a string":         func(item map[string]any) { item["event"] = nil },
		"variables not an object":    func(item map[string]any) { item["variables"] = "x" },
		"variables an array":         func(item map[string]any) { item["variables"] = []any{"x"} },
		"variables value not string": func(item map[string]any) { item["variables"] = map[string]any{"OTHER": 1} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			fw := &fakeWoodpecker{
				replies: []int{http.StatusBadGateway, http.StatusNoContent},
				list: func(string) []map[string]any {
					item := dispatchEntry(1, dispatchStart.Unix(), "someone-else")
					mutate(item)
					return []map[string]any{item}
				},
			}
			ctx := newFakeWoodpecker(t, fw)
			clock := &fakeClock{now: dispatchStart}

			_, err := dispatchCDTargets(t.Context(), ctx, []string{"first"}, testDispatchID, "", 0, clock.seam())
			if err == nil || !strings.Contains(err.Error(), "could not confirm the CD dispatch") {
				t.Fatalf("err = %v, want a could-not-confirm failure", err)
			}
			if got := fw.counts().posts; got != 1 {
				t.Fatalf("posts = %d, want 1 (a malformed item must not trigger a retry)", got)
			}
		})
	}
}
