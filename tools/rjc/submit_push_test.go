package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// pushedHeadFixture is an owned bookmark on the fixture forge; after a push, GET /pulls/1 answers heads[i] on read i.
// The last head repeats. Pre-push reads fall through to the fixture.
func pushedHeadFixture(t *testing.T, heads func(bookmark, tip string) []Metadata) (*fixture, preparedChange, *atomic.Int32, *[]time.Duration) {
	t.Helper()
	f := parallelFixtureRepo(t)
	bookmark := f.new(t, "EX-1", "feat(rjc): confirm pushed head", "")
	f.commit(t, "next.txt")
	jjTest(t, f.repo, "bookmark", "move", bookmark, "--to", "@-")
	tip := jjTest(t, f.repo, "log", "--no-graph", "-r", quoteRev(bookmark), "-T", "commit_id")
	f.prs[bookmark] = 1
	f.metadata[1] = Metadata{HeadRef: bookmark, HeadSHA: tip, BaseRef: f.repo.profile.Trunk}
	sequence := heads(bookmark, tip)
	reads := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if len(f.pushes) > 0 && req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls/1") {
			i := min(int(reads.Add(1))-1, len(sequence)-1)
			served := sequence[i]
			served.Title, served.Body = f.metadata[1].Title, f.metadata[1].Body
			respondJSON(t, w, githubMetadata(served))
			return
		}
		f.serve(t, w, req)
	}))
	t.Cleanup(server.Close)
	s, err := service(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.forge = &forgeClient{baseURL: server.URL, client: server.Client()}
	var waits []time.Duration
	f.repo.pushedHeadWait = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	item := preparedChange{change: Change{Bookmark: bookmark, Tip: tip, Base: mainOrigin}, pr: 1, before: f.metadata[1]}
	return f, item, reads, &waits
}

// pushedCandidate stages the candidate, the PR body and the published baseline so reconcile
// sees no human edit. The PR body and the baseline both start at the candidate's authored body.
func pushedCandidate(t *testing.T, f *fixture, item *preparedChange) {
	t.Helper()
	candidate, err := f.repo.prepareCandidate(f.ctx, item.change.Bookmark, "feat(rjc): confirm pushed head", f.bodyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	candidate.path, err = (fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}).candidate(item.change.Bookmark, candidate.body)
	if err != nil {
		t.Fatal(err)
	}
	item.candidate = candidate
	s, err := service(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.store = fileBaseline{root: f.repo.stateDir, keys: f.repo.keys}
	if err := s.store.Commit(item.change.Bookmark, Published{Title: candidate.title, Authored: strings.TrimSpace(candidate.body) + "\n"}); err != nil {
		t.Fatal(err)
	}
	item.baseline = Published{Title: candidate.title, Authored: strings.TrimSpace(candidate.body) + "\n"}
	f.metadata[1] = Metadata{Title: candidate.title, Body: candidate.body, HeadRef: item.change.Bookmark, HeadSHA: item.change.Tip, BaseRef: f.repo.profile.Trunk}
}

func TestPushLagThenMatchSucceeds(t *testing.T) {
	t.Parallel()
	const stale = "0000000000000000000000000000000000000000"
	f, item, _, waits := pushedHeadFixture(t, func(bookmark, tip string) []Metadata {
		lagging := Metadata{HeadRef: bookmark, HeadSHA: stale, BaseRef: "main"}
		settled := Metadata{HeadRef: bookmark, HeadSHA: tip, BaseRef: "main"}
		return []Metadata{lagging, lagging, settled}
	})
	pushedCandidate(t, f, &item)
	if err := f.repo.pushChange(f.ctx, &item, submitOptions{}, f, io.Discard); err != nil {
		t.Fatalf("lagging head must confirm once it matches, got %v", err)
	}
	if got := len(*waits); got != 2 {
		t.Fatalf("confirmation waits = %d, want 2: the third head read must settle", got)
	}
	if !slices.Equal(*waits, pushedHeadBackoff[:2]) {
		t.Fatalf("waits = %v, want %v", *waits, pushedHeadBackoff[:2])
	}
	entry, found, err := (Ledger{root: f.repo.stateDir}).Find(f.repo.profile.Name, item.change.Bookmark)
	if err != nil || !found || entry.PR != 1 {
		t.Fatalf("ledger = %+v found=%v err=%v, want PR 1 recorded", entry, found, err)
	}
}

func TestUnconfirmedPushNamesObservedHead(t *testing.T) {
	t.Parallel()
	const observed = "1111111111111111111111111111111111111111"
	f, item, reads, waits := pushedHeadFixture(t, func(bookmark, _ string) []Metadata {
		return []Metadata{{HeadRef: bookmark, HeadSHA: observed, BaseRef: "main"}}
	})
	err := f.repo.pushChange(f.ctx, &item, submitOptions{}, f, io.Discard)
	assertRefusalGate(t, err, "unconfirmed-push")
	if !strings.Contains(err.Error(), observed) {
		t.Fatalf("refusal must name the observed head %s: %v", observed, err)
	}
	if got := reads.Load(); got != int32(len(pushedHeadBackoff)+1) {
		t.Fatalf("metadata reads = %d, want %d", got, len(pushedHeadBackoff)+1)
	}
	if !slices.Equal(*waits, pushedHeadBackoff) {
		t.Fatalf("waits = %v, want %v", *waits, pushedHeadBackoff)
	}
}

func TestUnconfirmedPushNamesObservedRefWhenHeadMatches(t *testing.T) {
	t.Parallel()
	const otherRef = "home/ex-9-other"
	f, item, _, _ := pushedHeadFixture(t, func(_, tip string) []Metadata {
		return []Metadata{{HeadRef: otherRef, HeadSHA: tip, BaseRef: "main"}}
	})
	err := f.repo.pushChange(f.ctx, &item, submitOptions{}, f, io.Discard)
	assertRefusalGate(t, err, "unconfirmed-push")
	if !strings.Contains(err.Error(), otherRef) {
		t.Fatalf("refusal must name the observed ref %s: %v", otherRef, err)
	}
}

func TestPushCancelledHeadWaitIsInterruptFailure(t *testing.T) {
	t.Parallel()
	const stale = "2222222222222222222222222222222222222222"
	f, item, reads, _ := pushedHeadFixture(t, func(bookmark, _ string) []Metadata {
		return []Metadata{{HeadRef: bookmark, HeadSHA: stale, BaseRef: "main"}}
	})
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	f.repo.pushedHeadWait = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	err := f.repo.pushChange(ctx, &item, submitOptions{}, f, io.Discard)
	detail, ok := errors.AsType[*failure](err)
	if !ok || detail.gate != toolFailureGate || !strings.Contains(detail.reason, "interrupted while confirming push of PR 1") {
		t.Fatalf("cancelled wait must be an interrupt failure naming PR 1, got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupt failure must keep the cause, got %v", err)
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("metadata reads = %d, want 1 before the cancelled wait", got)
	}
}

func TestPushMetadataErrorIsNotRetried(t *testing.T) {
	t.Parallel()
	f := parallelFixtureRepo(t)
	reads := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		reads.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		respondJSON(t, w, map[string]string{"message": "fixture read failure"})
	}))
	t.Cleanup(server.Close)
	var waits []time.Duration
	f.repo.pushedHeadWait = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	forge := &forgeClient{baseURL: server.URL, client: server.Client()}
	_, err := f.repo.awaitPushedHead(f.ctx, forge, 1, Change{Bookmark: "home/ex-1-test", Tip: "a", Base: mainOrigin})
	if err == nil {
		t.Fatal("a metadata read error must be returned")
	}
	if got := reads.Load(); got != 1 || len(waits) != 0 {
		t.Fatalf("reads = %d, waits = %v; a read error must not retry", got, waits)
	}
}
