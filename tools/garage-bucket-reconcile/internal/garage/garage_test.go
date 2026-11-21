package garage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Mutation pairs for the Record and StringArray guards. Each pair has an input that must pass and one that must fail;
// a mutant that loosens a guard (accepting arrays as records, or non-strings in string lists) fails the second case.

func TestRecordGuardMutationPair(t *testing.T) {
	t.Parallel()
	// Accepts: a JSON object.
	if _, ok := Record(map[string]any{"id": "x"}); !ok {
		t.Error("Record rejected a JSON object")
	}
	// Rejects: null, an array, a string and a number. A guard that accepts any of these lets bad input through.
	for _, v := range []any{nil, []any{}, "text", 42.0} {
		if _, ok := Record(v); ok {
			t.Errorf("Record accepted %#v", v)
		}
	}
}

func TestStringArrayGuardMutationPair(t *testing.T) {
	t.Parallel()
	// Accepts: an array of strings, including empty.
	if got, ok := StringArray([]any{"a", "b"}); !ok || len(got) != 2 {
		t.Errorf("StringArray rejected ['a','b']: %v %v", got, ok)
	}
	if _, ok := StringArray([]any{}); !ok {
		t.Error("StringArray rejected an empty array")
	}
	// Rejects: a non-array, and an array with a non-string element.
	if _, ok := StringArray("a"); ok {
		t.Error("StringArray accepted a string")
	}
	if _, ok := StringArray([]any{"a", 1.0}); ok {
		t.Error("StringArray accepted a mixed array")
	}
}

func TestErrorBodyRedactsTokenAndTruncatesToUTF16Limit(t *testing.T) {
	t.Parallel()
	if got := ErrorBody("failure secret-token", "secret-token"); got != "failure [redacted]" {
		t.Errorf("ErrorBody = %q", got)
	}
	long := strings.Repeat("a", 250)
	got := ErrorBody(long, "")
	if got != strings.Repeat("a", 200)+"…" {
		t.Errorf("ErrorBody did not truncate to 200 with an ellipsis: len %d", len(got))
	}
	// 199 ASCII bytes, then é (1 UTF-16 unit, 2 bytes) and 😀 (2 UTF-16 units, 4 bytes).
	// UTF-16 truncation to 200 units keeps the é and drops the emoji. Byte truncation would differ.
	mixed := strings.Repeat("a", 199) + "é😀"
	if got := ErrorBody(mixed, ""); got != strings.Repeat("a", 199)+"é…" {
		t.Errorf("ErrorBody(mixed) = %q, want 199 a, é, and the ellipsis (UTF-16 units)", got)
	}
}

func TestTrimTrailingSlashesRemovesEverySlash(t *testing.T) {
	t.Parallel()
	if got := TrimTrailingSlashes("http://garage.example.com:3903///"); got != "http://garage.example.com:3903" {
		t.Errorf("TrimTrailingSlashes = %q", got)
	}
}

func TestDecodeJSONRejectsEmptyBody(t *testing.T) {
	t.Parallel()
	if _, err := DecodeJSON("GetKeyInfo", []byte("")); err == nil {
		t.Error("DecodeJSON accepted an empty body")
	}
}

// stubDoer is an http.RoundTripper that returns one fixed status, or a transport error.
type stubDoer struct {
	status int
	err    error
}

func (s stubDoer) RoundTrip(req *http.Request) (*http.Response, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &http.Response{StatusCode: s.status, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

// stubClient wraps a stubDoer in the http.Client that Deps.HTTP takes.
func stubClient(s stubDoer) *http.Client {
	return &http.Client{Transport: s}
}

func TestWaitForAdminReturnsOnceHealthIsOK(t *testing.T) {
	t.Parallel()
	deps := Deps{HTTP: stubClient(stubDoer{status: http.StatusOK}), Sleep: noSleep, Now: time.Now, Log: func(string) {}}
	if err := WaitForAdmin(context.Background(), "http://garage", time.Second, deps); err != nil {
		t.Fatalf("WaitForAdmin: %v", err)
	}
}

func TestWaitForAdminRejectsNegativeTimeout(t *testing.T) {
	t.Parallel()
	deps := Deps{HTTP: stubClient(stubDoer{status: http.StatusOK}), Sleep: noSleep, Now: time.Now, Log: func(string) {}}
	if err := WaitForAdmin(context.Background(), "http://garage", -time.Second, deps); err == nil {
		t.Error("WaitForAdmin accepted a negative timeout")
	}
}

func TestSendPassesTransportErrorThrough(t *testing.T) {
	t.Parallel()
	boom := errors.New("connection reset")
	deps := Deps{HTTP: stubClient(stubDoer{err: boom}), Sleep: noSleep, Now: time.Now, Log: func(string) {}}
	if _, _, err := Send(context.Background(), deps, "http://garage", "ListBuckets", http.MethodGet, "t", nil); !errors.Is(err, boom) {
		t.Errorf("err = %v, want connection reset", err)
	}
}

// TestSendDoesNotFollowRedirects: a 3xx from the admin API must not make the client resend
// the admin token to the redirect target. The target must receive no request, and Send
// must return the 3xx status with no error.
func TestSendDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			var targetRequests atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				targetRequests.Add(1)
			}))
			defer target.Close()
			redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+r.URL.Path, status)
			}))
			defer redirector.Close()

			deps := Deps{HTTP: &http.Client{}, Sleep: noSleep, Now: time.Now, Log: func(string) {}}
			got, _, err := Send(t.Context(), deps, redirector.URL, "ListBuckets", http.MethodGet, "admin-token-value", nil)
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if got != status {
				t.Errorf("status = %d, want %d", got, status)
			}
			if n := targetRequests.Load(); n != 0 {
				t.Errorf("redirect target received %d requests, want 0", n)
			}
		})
	}
}

func noSleep(context.Context, time.Duration) error { return nil }
