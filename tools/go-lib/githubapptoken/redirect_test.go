package githubapptoken

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMintNeverFollowsRedirect(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var targetHits atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetHits.Add(1)
			}))
			defer target.Close()
			var bearer atomic.Value
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); got != "" {
					bearer.Store(got)
				}
				http.Redirect(w, r, target.URL+"/steal", status)
			}))
			defer origin.Close()
			client := &http.Client{}
			client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "api.github.com" {
					return rewriteGitHubURL{base: origin.URL, next: origin.Client().Transport}.RoundTrip(r)
				}
				return origin.Client().Transport.RoundTrip(r)
			})

			_, err := mintWithClient(t.Context(), "Iv1.client", "example-org/example-repo", privateKey, client)
			if err == nil {
				t.Fatal("mint succeeded through a redirect")
			}
			if hits := targetHits.Load(); hits != 0 {
				t.Fatalf("redirect target got %d requests, want 0", hits)
			}
			if jwt, _ := bearer.Load().(string); jwt == "" || strings.Contains(err.Error(), jwt) {
				t.Fatalf("error leaks the bearer token or none was sent: %v", err)
			}
			if client.CheckRedirect != nil {
				t.Fatal("caller's client CheckRedirect was modified")
			}
		})
	}
}
