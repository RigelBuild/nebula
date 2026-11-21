package woodpecker

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestFetchJSONDoesNotFollowRedirects: a 3xx from the API must not make the
// client resend the CD token to the redirect target. Go keeps Authorization on a
// redirect to the same host on another port, so the target on the same host must
// receive no request, and the call must fail with the 3xx status.
func TestFetchJSONDoesNotFollowRedirects(t *testing.T) {
	statuses := []struct {
		name   string
		status int
	}{
		{name: "301 moved permanently", status: http.StatusMovedPermanently},
		{name: "302 found", status: http.StatusFound},
		{name: "307 temporary redirect", status: http.StatusTemporaryRedirect},
		{name: "308 permanent redirect", status: http.StatusPermanentRedirect},
	}
	modes := []struct {
		name   string
		inject bool
	}{
		{name: "nil client", inject: false},
		{name: "injected client", inject: true},
	}
	for _, tc := range statuses {
		for _, mode := range modes {
			t.Run(tc.name+" with "+mode.name, func(t *testing.T) {
				var targetRequests, targetAuthorized atomic.Int32
				target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					targetRequests.Add(1)
					if r.Header.Get("Authorization") != "" {
						targetAuthorized.Add(1)
					}
				}))
				defer target.Close()
				redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, target.URL+r.URL.Path, tc.status)
				}))
				defer redirector.Close()

				client := &Client{BaseURL: redirector.URL, Token: "token-value"}
				if mode.inject {
					client.HTTP = &http.Client{}
				}
				_, err := client.FetchJSON(t.Context(), http.MethodGet, "/api/repos/1/pipelines", nil, 0)

				if n := targetRequests.Load(); n != 0 {
					t.Errorf("redirect target received %d requests, %d with Authorization; want 0", n, targetAuthorized.Load())
				}
				statusErr, ok := errors.AsType[*StatusError](err)
				if !ok || statusErr.Status != tc.status {
					t.Fatalf("FetchJSON() error = %v, want StatusError %d", err, tc.status)
				}
				if strings.Contains(err.Error(), "token-value") {
					t.Errorf("FetchJSON() error %q contains the CD token", err)
				}
			})
		}
	}
}
