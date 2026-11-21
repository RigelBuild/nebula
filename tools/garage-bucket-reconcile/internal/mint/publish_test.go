package mint

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/fakeadmin"
)

func TestPublishesEmptySecretBeforeDeletingOldTokensWithSameName(t *testing.T) {
	t.Parallel()
	secrets := newFakeSecret(secret("", "rv-empty"))
	var events []string
	listed := []any{
		tokenRecord(new("old-token-id"), Scope, nil, tokenName),
		tokenRecordNullExpiry(new("other-name-id"), Scope, "some-other-token"),
	}
	respond := emptyResponder(listed)
	d := fakeadmin.NewDoer(func(req fakeadmin.Request) (fakeadmin.Reply, error) {
		switch apiEndpoint(req) {
		case "CreateAdminToken":
			if req.Authorization != "Bearer "+masterTok {
				return fakeadmin.Reply{}, errors.New("create must use the master token")
			}
			events = append(events, "create")
			want := map[string]any{
				"name":       tokenName,
				"scope":      []string{"ListBuckets", "CreateBucket", "GetKeyInfo", "UpdateBucket", "AllowBucketKey"},
				"expiration": isoAfter(TTL),
			}
			if !jsonEqualAny(req.Body, want) {
				return fakeadmin.Reply{}, errors.New("create body mismatch")
			}
		case "ListAdminTokens":
			events = append(events, "list")
		case "DeleteAdminToken":
			events = append(events, "delete:"+deleteID(req))
		}
		return respond(req)
	})
	result, err := Run(context.Background(), testConfig(), testDeps(d, fixedClock(), secrets))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "minted" {
		t.Errorf("result = %q, want minted", result)
	}
	writes := secrets.Writes()
	if len(writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(writes))
	}
	if rv := mapAt(writes[0], "metadata")["resourceVersion"]; rv != "rv-empty" {
		t.Errorf("resourceVersion = %v, want rv-empty", rv)
	}
	wantToken := base64.StdEncoding.EncodeToString([]byte("new-id.new-secret"))
	if got := mapAt(writes[0], "data")["token"]; got != wantToken {
		t.Errorf("data.token = %v, want %v", got, wantToken)
	}
	ann := mapAt(mapAt(writes[0], "metadata"), "annotations")
	if ann["kustomize.toolkit.fluxcd.io/ssa"] != "IfNotPresent" {
		t.Errorf("annotation dropped: %v", ann)
	}
	if !slices.Equal(events, []string{"create", "list", "delete:old-token-id"}) {
		t.Errorf("events = %v", events)
	}
	for _, r := range d.Requests() {
		if strings.Contains(r.URL, "other-name-id") {
			t.Error("deleted a token with another name")
		}
	}
}

func TestRetainsMintedTokenAfterFailedPublishAndRemovesItAfterLaterPublish(t *testing.T) {
	t.Parallel()
	tokens := []map[string]any{
		tokenRecord(new("token-1"), Scope, nil, tokenName),
		tokenRecord(new("token-2"), Scope, nil, tokenName),
	}
	var mu sync.Mutex
	var deleted []string
	createCount := 0
	secrets := &flakySecret{inner: newFakeSecret(emptySecret())}
	d := fakeadmin.NewDoer(func(req fakeadmin.Request) (fakeadmin.Reply, error) {
		if r, ok := healthReady(req); ok {
			return r, nil
		}
		switch apiEndpoint(req) {
		case "CreateAdminToken":
			mu.Lock()
			createCount++
			n := createCount
			mu.Unlock()
			r, ok, err := createdTokenReply(req, "token-"+strconv.Itoa(n), "secret-"+strconv.Itoa(n))
			if !ok {
				return fakeadmin.Reply{}, errors.New("expected CreateAdminToken")
			}
			return r, err
		case "ListAdminTokens":
			return fakeadmin.Reply{Body: tokens}, nil
		case "DeleteAdminToken":
			mu.Lock()
			defer mu.Unlock()
			if id := deleteID(req); id != "" {
				deleted = append(deleted, id)
			}
			return fakeadmin.Reply{}, nil
		}
		return fakeadmin.Reply{}, errors.New("unexpected request " + req.URL)
	})
	clock := fixedClock()
	if _, err := Run(context.Background(), testConfig(), testDeps(d, clock, secrets)); err == nil || err.Error() != "publish failed" {
		t.Fatalf("first Run err = %v, want publish failed", err)
	}
	if len(deleted) != 0 {
		t.Errorf("deleted after failed publish = %v, want none", deleted)
	}
	// Token literals: created is the fake now, and expiration is now plus the 30-day TTL.
	wantToken := func(id string) map[string]any {
		return map[string]any{
			"id":         id,
			"created":    "2026-01-01T00:00:00.000Z",
			"name":       tokenName,
			"expiration": "2026-01-31T00:00:00.000Z",
			"expired":    false,
			"scope":      Scope,
		}
	}
	if !jsonEqualAny(tokens[0], wantToken("token-1")) || !jsonEqualAny(tokens[1], wantToken("token-2")) {
		t.Error("token list was mutated")
	}
	if _, err := Run(context.Background(), testConfig(), testDeps(d, clock, secrets)); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if !slices.Equal(deleted, []string{"token-1"}) {
		t.Errorf("deleted = %v, want [token-1]", deleted)
	}
}

func TestConflict409DeletesOnlyThisRunsTokenAndReturnsSuccessfully(t *testing.T) {
	t.Parallel()
	secrets := conflictSecret{inner: newFakeSecret(emptySecret())}
	d := fakeadmin.NewDoer(func(req fakeadmin.Request) (fakeadmin.Reply, error) {
		if r, ok := healthReady(req); ok {
			return r, nil
		}
		if r, ok, err := createdTokenReply(req, "own-id", "own-secret"); ok {
			return r, err
		}
		if apiEndpoint(req) == "DeleteAdminToken" {
			return fakeadmin.Reply{}, nil
		}
		return fakeadmin.Reply{}, errors.New("unexpected request " + req.URL)
	})
	result, err := Run(context.Background(), testConfig(), testDeps(d, fixedClock(), secrets))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result != "minted" {
		t.Errorf("result = %q, want minted", result)
	}
	var deleted []string
	for _, r := range d.Requests() {
		if apiEndpoint(r) == "DeleteAdminToken" {
			deleted = append(deleted, deleteID(r))
		}
	}
	if !slices.Equal(deleted, []string{"own-id"}) {
		t.Errorf("deleted = %v, want [own-id]", deleted)
	}
	if containsEndpoint(d.Requests(), "ListAdminTokens") {
		t.Error("ListAdminTokens was called after a conflict")
	}
}

// overlapState is what the two racing runs record between them: tokens they created and ids they deleted.
type overlapState struct {
	mu      sync.Mutex
	tokens  []map[string]any
	deleted []string
}

// overlapResponder answers one racing run. Its CreateAdminToken appends the token to the shared listing,
// and its ListAdminTokens and DeleteAdminToken read and change that listing.
func overlapResponder(label string, state *overlapState) func(fakeadmin.Request) (fakeadmin.Reply, error) {
	return func(req fakeadmin.Request) (fakeadmin.Reply, error) {
		if r, ok := healthReady(req); ok {
			return r, nil
		}
		switch apiEndpoint(req) {
		case "CreateAdminToken":
			id := "token-" + label
			state.mu.Lock()
			state.tokens = append(state.tokens, tokenRecord(new(id), Scope, nil, tokenName))
			state.mu.Unlock()
			r, ok, err := createdTokenReply(req, id, "secret-"+label)
			if !ok {
				return fakeadmin.Reply{}, errors.New("expected CreateAdminToken")
			}
			return r, err
		case "ListAdminTokens":
			state.mu.Lock()
			defer state.mu.Unlock()
			return fakeadmin.Reply{Body: slices.Clone(state.tokens)}, nil
		case "DeleteAdminToken":
			id := deleteID(req)
			state.mu.Lock()
			defer state.mu.Unlock()
			if id != "" {
				state.deleted = append(state.deleted, id)
				state.tokens = slices.DeleteFunc(state.tokens, func(tok map[string]any) bool { return tok["id"] == id })
			}
			return fakeadmin.Reply{}, nil
		}
		return fakeadmin.Reply{}, errors.New("unexpected request " + req.URL)
	}
}

// assertOverlapOutcome checks the survivor after two racing runs: the stored token, the one listed token,
// that the winner's token was never deleted, and that both publishes carried resourceVersion 17.
func assertOverlapOutcome(t *testing.T, winner string, race *raceSecret, state *overlapState) {
	t.Helper()
	winnerID := "token-" + winner
	if got := tokenFromSecret(race.Current()); got != "secret-"+winner {
		t.Errorf("stored token = %q, want secret-%s", got, winner)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.tokens) != 1 || state.tokens[0]["id"] != winnerID {
		t.Errorf("tokens = %v, want only %s", state.tokens, winnerID)
	}
	for _, id := range state.deleted {
		if id == winnerID {
			t.Error("winner's token was deleted")
		}
	}
	writes := race.Writes()
	if len(writes) != 2 {
		t.Fatalf("publishes = %d, want 2", len(writes))
	}
	for _, w := range writes {
		if mapAt(w, "metadata")["resourceVersion"] != "17" {
			t.Error("publish did not carry resourceVersion 17")
		}
	}
}

// runOverlap drives two concurrent mint runs against one racing Secret and checks the survivor.
func runOverlap(t *testing.T, winner string) {
	t.Helper()
	race := newRaceSecret(emptySecret(), winner)
	state := &overlapState{}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, label := range []string{"A", "B"} {
		wg.Go(func() {
			d := fakeadmin.NewDoer(overlapResponder(label, state))
			_, errs[i] = Run(context.Background(), testConfig(), testDeps(d, fixedClock(), race))
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	assertOverlapOutcome(t, winner, race, state)
}

// TestOverlappingRunsLeaveOnlyWinnersToken covers two mint runs racing to publish, for winners A and B.
func TestOverlappingRunsLeaveOnlyWinnersToken(t *testing.T) {
	t.Parallel()
	for _, winner := range []string{"A", "B"} {
		t.Run("winner "+winner, func(t *testing.T) {
			t.Parallel()
			runOverlap(t, winner)
		})
	}
}

// scopeResponder answers a single CreateAdminToken and records that its scope is exactly the five bucket scopes.
// Any other request is an error, so a stray call fails the run.
func scopeResponder(req fakeadmin.Request) (fakeadmin.Reply, error) {
	if r, ok := healthReady(req); ok {
		return r, nil
	}
	if r, ok, err := createdTokenReply(req, "scoped-id", "minted-secret-value"); ok {
		if err != nil {
			return fakeadmin.Reply{}, err
		}
		scope := scopeOf(req)
		want := []string{"ListBuckets", "CreateBucket", "GetKeyInfo", "UpdateBucket", "AllowBucketKey"}
		if !slices.Equal(scope, want) {
			return fakeadmin.Reply{}, errors.New("scope = " + strings.Join(scope, ","))
		}
		for _, bad := range []string{"CreateAdminToken", "UpdateAdminToken", "*"} {
			if slices.Contains(scope, bad) {
				return fakeadmin.Reply{}, errors.New("scope contains " + bad)
			}
		}
		return r, nil
	}
	if apiEndpoint(req) == "ListAdminTokens" {
		return fakeadmin.Reply{Body: []any{}}, nil
	}
	return fakeadmin.Reply{}, errors.New("unexpected request " + req.URL)
}

func TestRequestsFiveBucketScopesAndNeverLogsTokenValues(t *testing.T) {
	t.Parallel()
	secrets := newFakeSecret(emptySecret())
	var lines []string
	d := fakeadmin.NewDoer(scopeResponder)
	if _, err := Run(context.Background(), testConfig(), capturedDeps(d, fixedClock(), secrets, &lines)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	output := strings.Join(lines, " ")
	if strings.Contains(output, masterTok) {
		t.Error("master token appeared in logs")
	}
	if strings.Contains(output, "minted-secret-value") {
		t.Error("minted secret appeared in logs")
	}
	if !containsEndpoint(d.Requests(), "CreateAdminToken") {
		t.Error("CreateAdminToken was not called")
	}
}

func TestRedactsMasterTokenFromFailedCreateResponse(t *testing.T) {
	t.Parallel()
	secrets := newFakeSecret(emptySecret())
	d := fakeadmin.NewDoer(func(req fakeadmin.Request) (fakeadmin.Reply, error) {
		if r, ok := healthReady(req); ok {
			return r, nil
		}
		if apiEndpoint(req) == "CreateAdminToken" {
			return fakeadmin.Reply{Status: 500, Body: "error: " + masterTok}, nil
		}
		return fakeadmin.Reply{}, errors.New("unexpected request " + req.URL)
	})
	_, err := Run(context.Background(), testConfig(), testDeps(d, fixedClock(), secrets))
	if err == nil {
		t.Fatal("Run succeeded, want create failure")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("err = %v, want HTTP 500", err)
	}
	if strings.Contains(err.Error(), masterTok) {
		t.Error("master token leaked into the error")
	}
	if strings.Contains(err.Error(), "minted-secret-value") {
		t.Error("minted value leaked into the error")
	}
}
