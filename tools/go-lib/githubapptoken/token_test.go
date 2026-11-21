package githubapptoken

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type mintCase struct {
	name           string
	repo           string
	lookupStatus   int
	lookupBody     string
	tokenStatus    int
	tokenBody      string
	wantError      string
	wantMintError  string
	expectRequests int
	checkPKCS1     bool
}

func TestMint(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	for _, tt := range []mintCase{
		{name: "happy path: returns the installation token and carries the App JWT as Bearer on the installation GET", repo: "example-org/example-repo", expectRequests: 2},
		{name: "PKCS#1 fixture key flows through to createAppAuth's privateKey unchanged", repo: "example-org/example-repo", expectRequests: 2, checkPKCS1: true},
		{name: "fail-loud: installation GET 404 throws with an install/404 diagnosis", repo: "example-org/example-repo", lookupStatus: http.StatusNotFound, lookupBody: `{"message":"Not Found"}`, wantError: "not installed", expectRequests: 1},
		{name: "fail-loud: HTML/non-JSON body on a 200 throws a clear error, not an opaque JSON.parse crash", repo: "example-org/example-repo", lookupStatus: http.StatusOK, lookupBody: "<html><body>502 Bad Gateway</body></html>", wantError: "non-json body", expectRequests: 1},
		{name: "fail-loud: malformed repo (no owner/name) throws before any network call", repo: "no-slash", wantError: "owner/name"},
		{name: "fail-loud: multi-segment repo (owner/name/extra) throws before any network call", repo: "example-org/example-repo/extra", wantError: "owner/name"},
		{name: "fail-loud: 200 with a body missing a numeric id throws an install diagnosis", repo: "example-org/example-repo", lookupStatus: http.StatusOK, lookupBody: `{"message":"ok but no id"}`, wantError: "installation id", expectRequests: 1},
		{name: "fail-loud: installation GET 401 diagnoses a bad key (rotate)", repo: "example-org/example-repo", lookupStatus: http.StatusUnauthorized, lookupBody: `{"message":"Bad credentials"}`, wantError: "rotate", expectRequests: 1},
		{name: "fail-loud: installation GET 403 diagnoses a missing permission grant", repo: "example-org/example-repo", lookupStatus: http.StatusForbidden, lookupBody: `{"message":"Forbidden"}`, wantError: "grant", expectRequests: 1},
		{name: "fail-loud: installation token POST 403 returns no token", repo: "example-org/example-repo", tokenStatus: http.StatusForbidden, tokenBody: `{"message":"Forbidden"}`, wantMintError: "HTTP 403", expectRequests: 2},
		{name: "fail-loud: installation token POST malformed JSON returns no token", repo: "example-org/example-repo", tokenStatus: http.StatusCreated, tokenBody: "not json", wantMintError: "parse installation token response", expectRequests: 2},
		{name: "fail-loud: installation token POST empty token returns no token", repo: "example-org/example-repo", tokenStatus: http.StatusCreated, tokenBody: `{"token":""}`, wantMintError: "did not include a token", expectRequests: 2},
		{name: "fail-loud: installation token POST missing token returns no token", repo: "example-org/example-repo", tokenStatus: http.StatusCreated, tokenBody: `{"other":"value"}`, wantMintError: "did not include a token", expectRequests: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wantError != "" {
				testMintFailure(t, tt, privateKey)
				return
			}
			if tt.wantMintError != "" {
				testMintTokenFailure(t, tt, privateKey)
				return
			}
			testMintSuccess(t, tt, privateKey, key)
		})
	}
}

func testMintSuccess(t *testing.T, tt mintCase, privateKey []byte, key *rsa.PrivateKey) {
	t.Helper()
	var requests []*http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Clone(r.Context()))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/repos/example-org/example-repo/installation" {
			_, _ = io.WriteString(w, `{"id":12345}`)
			return
		}
		status := tt.tokenStatus
		if status == 0 {
			status = http.StatusCreated
		}
		w.WriteHeader(status)
		body := tt.tokenBody
		if body == "" {
			body = `{"token":"ghs_known_installation_token"}`
		}
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	client := server.Client()
	client.Transport = rewriteGitHubURL{base: server.URL, next: client.Transport}
	token, err := mintWithClient(context.Background(), "Iv1.client", tt.repo, privateKey, client)
	if err != nil {
		t.Fatal(err)
	}
	if token != Token("ghs_known_installation_token") {
		t.Fatalf("Mint() token=%q", token)
	}
	if len(requests) != 2 {
		t.Fatalf("HTTP calls=%d, want lookup and mint", len(requests))
	}
	lookup, mint := requests[0], requests[1]
	if lookup.Method != http.MethodGet || lookup.URL.Path != "/repos/example-org/example-repo/installation" {
		t.Fatalf("lookup request=%s %s", lookup.Method, lookup.URL.Path)
	}
	if mint.Method != http.MethodPost || mint.URL.Path != "/app/installations/12345/access_tokens" {
		t.Fatalf("mint request=%s %s", mint.Method, mint.URL.Path)
	}
	for _, request := range requests {
		if request.Header.Get("Accept") != "application/vnd.github+json" {
			t.Fatalf("Accept=%q", request.Header.Get("Accept"))
		}
		if request.Header.Get("X-Github-Api-Version") != "2022-11-28" {
			t.Fatalf("API version=%q", request.Header.Get("X-Github-Api-Version"))
		}
	}
	appJWT := strings.TrimPrefix(lookup.Header.Get("Authorization"), "Bearer ")
	if appJWT == lookup.Header.Get("Authorization") || mint.Header.Get("Authorization") != lookup.Header.Get("Authorization") {
		t.Fatal("App JWT bearer authorization is missing or changed between requests")
	}
	assertJWTClaims(t, appJWT, key)
	if tt.checkPKCS1 {
		block, _ := pem.Decode(privateKey)
		if block == nil || block.Type != "RSA PRIVATE KEY" {
			t.Fatalf("PKCS#1 fixture key block=%v", block)
		}
	}
}

func assertJWTClaims(t *testing.T, appJWT string, key *rsa.PrivateKey) {
	t.Helper()
	parsed, parseErr := jwt.Parse(appJWT, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodRS256 {
			t.Fatalf("JWT method=%v", token.Method)
		}
		return &key.PublicKey, nil
	})
	if parseErr != nil || parsed == nil || !parsed.Valid {
		t.Fatalf("App JWT valid=%v, error=%v", parsed != nil && parsed.Valid, parseErr)
	}
	var claims jwt.RegisteredClaims
	if _, _, err := new(jwt.Parser).ParseUnverified(appJWT, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Issuer != "Iv1.client" || claims.ExpiresAt == nil || claims.IssuedAt == nil {
		t.Fatalf("App JWT claims=%+v", claims)
	}
	if claims.IssuedAt.Time.Before(time.Now().Add(-70*time.Second)) || claims.IssuedAt.Time.After(time.Now().Add(-50*time.Second)) {
		t.Fatalf("App JWT issued-at outside 60-second clock-skew window: %v", claims.IssuedAt.Time)
	}
	if claims.ExpiresAt.Time.Before(time.Now().Add(8*time.Minute)) || claims.ExpiresAt.Time.After(time.Now().Add(10*time.Minute)) {
		t.Fatalf("App JWT expiry outside 9-minute validity window: %v", claims.ExpiresAt.Time)
	}
}

func testMintFailure(t *testing.T, tt mintCase, privateKey []byte) {
	t.Helper()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		status := tt.lookupStatus
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, tt.lookupBody)
	}))
	defer server.Close()
	client := server.Client()
	client.Transport = rewriteGitHubURL{base: server.URL, next: client.Transport}
	_, err := mintWithClient(context.Background(), "Iv1.client", tt.repo, privateKey, client)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), tt.wantError) {
		t.Fatalf("Mint() error=%v, want %q", err, tt.wantError)
	}
	if calls != tt.expectRequests {
		t.Fatalf("network calls=%d, want %d", calls, tt.expectRequests)
	}
}

func testMintTokenFailure(t *testing.T, tt mintCase, privateKey []byte) {
	t.Helper()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"id":12345}`)
			return
		}
		status := tt.tokenStatus
		if status == 0 {
			status = http.StatusCreated
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, tt.tokenBody)
	}))
	defer server.Close()
	client := server.Client()
	client.Transport = rewriteGitHubURL{base: server.URL, next: client.Transport}
	token, err := mintWithClient(context.Background(), "Iv1.client", tt.repo, privateKey, client)
	if token != "" {
		t.Fatalf("Mint() token=%q on error", token)
	}
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.wantMintError)) {
		t.Fatalf("Mint() error=%v, want %q", err, tt.wantMintError)
	}
	if calls != tt.expectRequests {
		t.Fatalf("network calls=%d, want %d", calls, tt.expectRequests)
	}
}

type rewriteGitHubURL struct {
	base string
	next http.RoundTripper
}

func (r rewriteGitHubURL) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(r.base, "http://")
	return r.next.RoundTrip(clone)
}

func TestMintScopedRequest(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	permissions := map[string]string{"statuses": "write"}
	var tokenRequest struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	var routes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routes = append(routes, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			if _, err := io.WriteString(w, `{"id":12345}`); err != nil {
				t.Errorf("write installation response: %v", err)
			}
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&tokenRequest); err != nil {
			t.Errorf("decode token request: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		if _, err := io.WriteString(w, `{"token":"ghs_scoped"}`); err != nil {
			t.Errorf("write token response: %v", err)
		}
	}))
	defer server.Close()
	client := server.Client()
	client.Transport = rewriteGitHubURL{base: server.URL, next: client.Transport}
	token, err := MintScopedWithClient(context.Background(), "Iv1.client", "example-org/example-repo", pemBytes, permissions, client)
	if err != nil {
		t.Fatal(err)
	}
	if token != Token("ghs_scoped") {
		t.Fatalf("MintScopedWithClient() token=%q", token)
	}
	if len(tokenRequest.Repositories) != 1 || tokenRequest.Repositories[0] != "example-repo" {
		t.Fatalf("repositories=%v, want only [example-repo]", tokenRequest.Repositories)
	}
	if len(tokenRequest.Permissions) != 1 || tokenRequest.Permissions["statuses"] != "write" {
		t.Fatalf("permissions=%v, want only statuses:write", tokenRequest.Permissions)
	}
	if len(routes) != 2 || routes[0] != "GET /repos/example-org/example-repo/installation" || routes[1] != "POST /app/installations/12345/access_tokens" {
		t.Fatalf("requests=%v, want installation lookup followed by token creation", routes)
	}
}

func TestMintScopedErrorDoesNotExposePEM(t *testing.T) {
	privateKey := []byte("-----BEGIN PRIVATE KEY-----\nPEM-SECRET-SENTINEL\n-----END PRIVATE KEY-----")
	_, err := MintScoped(context.Background(), "Iv1.client", "example-org/example-repo", privateKey, map[string]string{"statuses": "write"})
	if err == nil {
		t.Fatal("MintScoped() error=nil, want invalid-key error")
	}
	if strings.Contains(err.Error(), string(privateKey)) || strings.Contains(err.Error(), "PEM-SECRET-SENTINEL") {
		t.Fatalf("MintScoped() error exposed private key material: %v", err)
	}
}

func TestMintScopedRequiresPermissionsBeforeNetwork(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	tests := []struct {
		name        string
		permissions map[string]string
	}{
		{name: "nil permissions"},
		{name: "empty permissions", permissions: map[string]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("unexpected network request")
			})}
			_, err := mintScopedWithClient(context.Background(), "Iv1.client", "example-org/example-repo", pemBytes, tt.permissions, client)
			if err == nil {
				t.Fatal("MintScoped() error=nil, want missing_permissions")
			}
			var coded interface{ Code() any }
			if !errors.As(err, &coded) || coded.Code() != "missing_permissions" {
				t.Fatalf("MintScoped() error=%v, want code missing_permissions", err)
			}
			if calls != 0 {
				t.Fatalf("network calls=%d, want 0", calls)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestKeyFileReadPicksUpRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app-key.pem")
	if err := os.WriteFile(path, []byte("first-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyFile := NewKeyFile(path)
	first, err := keyFile.Read()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != "first-key" {
		t.Fatalf("first key = %q, want first-key", first)
	}
	if err := os.WriteFile(path, []byte("rotated-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	rotated, err := keyFile.Read()
	if err != nil {
		t.Fatal(err)
	}
	if string(rotated) != "rotated-key" {
		t.Fatalf("rotated key = %q, want rotated-key", rotated)
	}
}
