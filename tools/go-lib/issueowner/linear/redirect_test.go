package linear

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestIssueClientNeverFollowsRedirect(t *testing.T) {
	const token = "lin_secret_token_value"
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var targetHits atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetHits.Add(1)
			}))
			defer target.Close()
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+"/steal", status)
			}))
			defer origin.Close()
			client := &http.Client{Transport: origin.Client().Transport}

			api := NewIssueAPIWithToken(client, origin.URL, token)
			_, err := api.FetchIssue(t.Context(), "RIG-42")
			if err == nil {
				t.Fatal("request succeeded through a redirect")
			}
			if hits := targetHits.Load(); hits != 0 {
				t.Fatalf("redirect target got %d requests, want 0", hits)
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("error leaks the token: %v", err)
			}
			if client.CheckRedirect != nil {
				t.Fatal("caller's client CheckRedirect was modified")
			}

			apiKeyAPI, err := NewIssueAPI(t.Context(), client, origin.URL, func(name string) string {
				if name == "LINEAR_API_KEY" {
					return token
				}
				return ""
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := apiKeyAPI.FetchIssue(t.Context(), "RIG-42"); err == nil || strings.Contains(err.Error(), token) {
				t.Fatalf("NewIssueAPI redirect err = %v", err)
			}
			if hits := targetHits.Load(); hits != 0 {
				t.Fatalf("redirect target got %d requests via NewIssueAPI, want 0", hits)
			}
			if client.CheckRedirect != nil {
				t.Fatal("caller's client CheckRedirect was modified by NewIssueAPI")
			}
		})
	}
}
