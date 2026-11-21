package woodpecker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// testInClusterURL is the in-cluster Woodpecker API address the tests supply.
const testInClusterURL = "https://ci.example.com"

// deadlineRecorder is an http.RoundTripper that records the deadline of each
// request it sees and answers 204 without touching the network.
type deadlineRecorder struct {
	deadline time.Time
	hasDL    bool
}

func (d *deadlineRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	d.deadline, d.hasDL = req.Context().Deadline()
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

// TestParseBoundedIntBounds pins the shared env rule: digits only, a safe
// integer, and at least min. The error names the variable, as the TS did.
func TestParseBoundedIntBounds(t *testing.T) {
	ok := []struct {
		value string
		min   int64
		want  int64
	}{
		{"0", 0, 0},
		{"1", 1, 1},
		{"9007199254740991", 1, 9007199254740991},
	}
	for _, c := range ok {
		got, err := ParseBoundedInt(c.value, "X", c.min)
		if err != nil || got != c.want {
			t.Fatalf("ParseBoundedInt(%q, min %d) = %d, %v; want %d", c.value, c.min, got, err, c.want)
		}
	}
	bad := []struct {
		value string
		min   int64
	}{
		{"", 0}, {"0", 1}, {"-1", 0}, {"1.5", 0}, {"1e3", 0}, {" 7", 0}, {"x", 0},
		{"9007199254740992", 0},
	}
	for _, c := range bad {
		_, err := ParseBoundedInt(c.value, "CI_PIPELINE_NUMBER", c.min)
		if err == nil || err.Error() != "invalid CI_PIPELINE_NUMBER" {
			t.Fatalf("ParseBoundedInt(%q, min %d) err = %v; want \"invalid CI_PIPELINE_NUMBER\"", c.value, c.min, err)
		}
	}
}

func TestDispatchCDTargetsSendsSelectedTargets(t *testing.T) {
	var method string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	_, err := DispatchCDTargets(context.Background(),
		Context{BaseURL: server.URL, Token: "token", RepoID: 1},
		[]string{"first", "second"}, testDispatchID, "a1b2c3d4", 0)
	if err != nil {
		t.Fatalf("DispatchCDTargets: %v", err)
	}
	if method != http.MethodPost {
		t.Fatalf("method = %s, want POST", method)
	}
	variables, _ := body["variables"].(map[string]any)
	nonce, _ := variables["PETREL_CD_DISPATCH_ID"].(string)
	if !dispatchNonce.MatchString(nonce) {
		t.Fatalf("PETREL_CD_DISPATCH_ID = %q, want a v4 UUID", nonce)
	}
	want := map[string]any{
		"branch":    "main",
		"variables": map[string]any{"PETREL_CD_TARGETS": "first,second", "PETREL_CD_DISPATCH_ID": nonce, "CD_CHECK_BASE": "a1b2c3d4"},
	}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("body = %v, want %v", body, want)
	}
}

func TestDispatchCDTargetsAbortsOnTimeout(t *testing.T) {
	// The handler holds the request open until the test releases it, so the
	// only way the client returns is by hitting its own timeout.
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)

	_, err := DispatchCDTargets(context.Background(),
		Context{BaseURL: server.URL, Token: "token", RepoID: 1},
		[]string{"first"}, testDispatchID, "", 10*time.Millisecond)
	if err == nil {
		t.Fatal("DispatchCDTargets returned nil, want a timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestFetchJSONReportsStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, Token: "t"}
	_, err := client.FetchJSON(context.Background(), http.MethodGet, "/x", nil, 0)
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Status != http.StatusTeapot {
		t.Fatalf("err = %v, want StatusError 418", err)
	}
}

func TestFetchJSONReturnsErrNoContent(t *testing.T) {
	client := &Client{BaseURL: "http://unused", Token: "t", HTTP: &http.Client{Transport: &deadlineRecorder{}}}
	_, err := client.FetchJSON(context.Background(), http.MethodGet, "/x", nil, 0)
	if !errors.Is(err, ErrNoContent) {
		t.Fatalf("FetchJSON error = %v, want ErrNoContent", err)
	}
}

func TestLookupRepoIDRejectsInvalidID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte(`{"id":0}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, Token: "t"}
	if _, err := client.LookupRepoID(context.Background(), "owner/repo"); err == nil {
		t.Fatal("LookupRepoID accepted id 0, want an error")
	}
}

func TestLookupRepoIDRejectsNoContent(t *testing.T) {
	client := &Client{BaseURL: "http://unused", Token: "t", HTTP: &http.Client{Transport: &deadlineRecorder{}}}
	if _, err := client.LookupRepoID(context.Background(), "owner/repo"); err == nil {
		t.Fatal("LookupRepoID accepted a 204 response without a repo id")
	}
}

func TestLookupRepoIDReturnsID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte(`{"id":42}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer server.Close()

	client := &Client{BaseURL: server.URL, Token: "t"}
	id, err := client.LookupRepoID(context.Background(), "owner/repo")
	if err != nil || id != 42 {
		t.Fatalf("LookupRepoID = %d, %v; want 42", id, err)
	}
}

// TestContextFromEnvWithRepoIDFallsBackToLookup: with CI_REPO_ID empty, the
// call takes the CI_REPO lookup path. The id must come from the server, so the
// test fails if the lookup is skipped.
func TestContextFromEnvWithRepoIDFallsBackToLookup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/repos/lookup/owner/repo" {
			t.Errorf("lookup path = %q, want /api/repos/lookup/owner/repo", r.URL.Path)
		}
		if _, err := w.Write([]byte(`{"id":77}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer server.Close()
	env := map[string]string{
		"WOODPECKER_API_URL":  server.URL,
		"WOODPECKER_CD_TOKEN": "tok",
		"CI_REPO":             "owner/repo",
	}
	get := func(key string) string { return env[key] }
	got, err := ContextFromEnvWithRepoID(context.Background(), get, testInClusterURL, server.Client())
	if err != nil {
		t.Fatalf("ContextFromEnvWithRepoID: %v", err)
	}
	if got.RepoID != 77 {
		t.Fatalf("RepoID = %d, want 77 from the lookup", got.RepoID)
	}
}

func TestContextFromEnvMissingTokenIsInvalidAPIContext(t *testing.T) {
	env := map[string]string{
		"WOODPECKER_API_URL":  "https://ci.example",
		"WOODPECKER_CD_TOKEN": "   ",
		"CI_REPO_ID":          "42",
	}
	get := func(key string) string { return env[key] }
	_, err := ContextFromEnvWithRepoID(context.Background(), get, testInClusterURL, nil)
	if !errors.Is(err, ErrMissingCredential) {
		t.Fatalf("err = %v, want ErrMissingCredential for a blank token", err)
	}
	if !errors.Is(err, ErrInvalidAPIContext) {
		t.Fatalf("err = %v, want it to wrap ErrInvalidAPIContext so the gate blocks", err)
	}
}

func TestContextFromEnvMalformedRepoIsInvalidAPIContext(t *testing.T) {
	env := map[string]string{
		"WOODPECKER_API_URL":  "https://ci.example",
		"WOODPECKER_CD_TOKEN": "tok",
		"CI_REPO":             "owner/repo/extra",
	}
	get := func(key string) string { return env[key] }
	_, err := ContextFromEnv(context.Background(), get, testInClusterURL, nil)
	if !errors.Is(err, ErrInvalidAPIContext) {
		t.Fatalf("err = %v, want ErrInvalidAPIContext for a malformed CI_REPO", err)
	}
}

func TestContextFromEnvTransientLookupFailureIsNotInvalidAPIContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	env := map[string]string{
		"WOODPECKER_API_URL":  server.URL,
		"WOODPECKER_CD_TOKEN": "tok",
		"CI_REPO":             "owner/repo",
	}
	get := func(key string) string { return env[key] }
	_, err := ContextFromEnv(context.Background(), get, testInClusterURL, server.Client())
	if err == nil {
		t.Fatal("a 503 from the repo lookup returned no error")
	}
	if errors.Is(err, ErrInvalidAPIContext) {
		t.Fatalf("a transient 503 was classified as an invalid API context: %v", err)
	}
}

// TestContextFromEnvRejectedCredentialIsInvalidAPIContext: with CI_REPO_ID
// unset, a 401 or 403 from the repo lookup means the server rejects the CD
// token. ContextFromEnv must wrap ErrInvalidAPIContext so the gate blocks.
func TestContextFromEnvRejectedCredentialIsInvalidAPIContext(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "rejected", status)
			}))
			defer server.Close()
			env := map[string]string{
				"WOODPECKER_API_URL":  server.URL,
				"WOODPECKER_CD_TOKEN": "tok",
				"CI_REPO":             "owner/repo",
			}
			get := func(key string) string { return env[key] }
			_, err := ContextFromEnv(context.Background(), get, testInClusterURL, server.Client())
			if !errors.Is(err, ErrInvalidAPIContext) {
				t.Fatalf("status %d: err = %v, want ErrInvalidAPIContext so the gate blocks", status, err)
			}
		})
	}
}

func TestContextFromEnvValidatesSchemeAndRepo(t *testing.T) {
	env := map[string]string{
		"WOODPECKER_API_URL":  "ftp://ci.example",
		"WOODPECKER_CD_TOKEN": "tok",
		"CI_REPO":             "owner/repo",
	}
	get := func(key string) string { return env[key] }
	if _, err := ContextFromEnv(context.Background(), get, testInClusterURL, nil); err == nil {
		t.Fatal("accepted a non-http scheme")
	}

	env["WOODPECKER_API_URL"] = "https://ci.example"
	env["CI_REPO"] = "owner/repo/extra"
	if _, err := ContextFromEnv(context.Background(), get, testInClusterURL, nil); err == nil {
		t.Fatal("accepted a malformed CI_REPO")
	}
}

// TestContextFromEnvIgnoresCIRepoID matches the TS cdApiContext that
// cd-reconcile uses: it reads CI_REPO only. A bare CI_REPO_ID is not enough.
func TestContextFromEnvIgnoresCIRepoID(t *testing.T) {
	env := map[string]string{
		"WOODPECKER_API_URL":  "https://ci.example",
		"WOODPECKER_CD_TOKEN": "tok",
		"CI_REPO_ID":          "42",
	}
	get := func(key string) string { return env[key] }
	if _, err := ContextFromEnv(context.Background(), get, testInClusterURL, nil); err == nil {
		t.Fatal("ContextFromEnv accepted CI_REPO_ID with no CI_REPO; the cd-reconcile path reads CI_REPO only")
	}
}

// TestContextFromEnvWithRepoIDUsesCIRepoIDWithoutLookup matches the TS run-cd:
// a set CI_REPO_ID is used directly, with no CI_REPO and no lookup.
func TestContextFromEnvWithRepoIDUsesCIRepoIDWithoutLookup(t *testing.T) {
	env := map[string]string{
		"WOODPECKER_API_URL":  "https://ci.example",
		"WOODPECKER_CD_TOKEN": "tok",
		"CI_REPO_ID":          "42",
	}
	get := func(key string) string { return env[key] }
	got, err := ContextFromEnvWithRepoID(context.Background(), get, testInClusterURL, nil)
	if err != nil {
		t.Fatalf("ContextFromEnvWithRepoID with CI_REPO_ID alone: %v; want it used directly", err)
	}
	if got.RepoID != 42 {
		t.Fatalf("RepoID = %d, want 42 from CI_REPO_ID", got.RepoID)
	}
}

// TestContextFromEnvWithRepoIDRejectsBadCIRepoID matches the TS parsePositiveInt
// rule. Only ContextFromEnvWithRepoID validates CI_REPO_ID.
func TestContextFromEnvWithRepoIDRejectsBadCIRepoID(t *testing.T) {
	for _, bad := range []string{"0", "-1", "1.5", "1e3", " 7 x", "x"} {
		env := map[string]string{
			"WOODPECKER_API_URL":  "https://ci.example",
			"WOODPECKER_CD_TOKEN": "tok",
			"CI_REPO_ID":          bad,
		}
		get := func(key string) string { return env[key] }
		if _, err := ContextFromEnvWithRepoID(context.Background(), get, testInClusterURL, nil); err == nil {
			t.Fatalf("CI_REPO_ID %q accepted, want an error", bad)
		}
	}
}

// TS: "uses the default timeout for target dispatch". A zero timeout must give
// the create POST the 180-second dispatch budget, not GateTimeout.
// It swaps http.DefaultClient.Transport, since dispatch has no client seam, so it must not run in parallel.
func TestDispatchCDTargetsUsesDefaultDispatchTimeout(t *testing.T) {
	recorder := &deadlineRecorder{}
	saved := http.DefaultClient.Transport
	http.DefaultClient.Transport = recorder
	t.Cleanup(func() { http.DefaultClient.Transport = saved })

	start := time.Now()
	if _, err := DispatchCDTargets(t.Context(), Context{BaseURL: "http://unused", Token: "t", RepoID: 1}, []string{"first"}, testDispatchID, "", 0); err != nil {
		t.Fatalf("DispatchCDTargets: %v", err)
	}
	if !recorder.hasDL {
		t.Fatal("create request had no deadline")
	}
	if got := recorder.deadline.Sub(start); got < 179*time.Second || got > 181*time.Second {
		t.Fatalf("create deadline = %v after start, want about 180s", got)
	}
}
