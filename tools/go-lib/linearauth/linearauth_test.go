package linearauth

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type rewriteTransport struct {
	base http.RoundTripper
	host string
}

func (t rewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.URL = &url.URL{Scheme: "http", Host: t.host, Path: request.URL.Path}
	clone.Host = t.host
	return t.base.RoundTrip(clone)
}

func testClient(server *httptest.Server) *http.Client {
	return &http.Client{Transport: rewriteTransport{base: server.Client().Transport, host: strings.TrimPrefix(server.URL, "http://")}}
}

func TestMintAppToken(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		scope         string
		wantToken     string
		wantErr       string
		wantScope     string
		wantRespScope string
		wantBasic     string
		wantMethod    string
		wantType      string
		wantExpiry    int
	}{
		{
			name:          "posts a client_credentials grant with Basic auth and returns the token",
			status:        http.StatusOK,
			body:          `{"access_token":"minted-token-1","token_type":"Bearer","scope":"read,write","expires_in":2592000}`,
			scope:         "read,write",
			wantToken:     "minted-token-1",
			wantScope:     "read,write",
			wantRespScope: "read,write",
			wantBasic:     "client-id-abc:client-secret-xyz",
			wantMethod:    http.MethodPost,
			wantType:      "Bearer",
			wantExpiry:    2592000,
		},
		{
			name:          "accepts legacy array-valued response scope",
			status:        http.StatusOK,
			body:          `{"access_token":"array-scope-token","scope":["read","write"]}`,
			scope:         "read",
			wantToken:     "array-scope-token",
			wantScope:     "read",
			wantRespScope: "read,write",
			wantBasic:     "client-id-abc:client-secret-xyz",
		},
		{
			name:      "accepts object-valued response scope",
			status:    http.StatusOK,
			body:      `{"access_token":"object-scope-token","scope":{"read":true}}`,
			scope:     "read",
			wantToken: "object-scope-token",
			wantScope: "read",
			wantBasic: "client-id-abc:client-secret-xyz",
		},
		{
			name:      "accepts null response scope",
			status:    http.StatusOK,
			body:      `{"access_token":"null-scope-token","scope":null}`,
			scope:     "read",
			wantToken: "null-scope-token",
			wantScope: "read",
			wantBasic: "client-id-abc:client-secret-xyz",
		},
		{
			name:      "allows a read-only app token scope",
			status:    http.StatusOK,
			body:      `{"access_token":"read-token"}`,
			scope:     "read",
			wantToken: "read-token",
			wantScope: "read",
			wantBasic: "client-id-abc:client-secret-xyz",
		},
		{
			name:      "throws with status and body when the token endpoint rejects",
			status:    http.StatusBadRequest,
			body:      `{"error":"invalid_client"}`,
			scope:     "read,write",
			wantErr:   "400 Bad Request\n{\"error\":\"invalid_client\"}",
			wantScope: "read,write",
			wantBasic: "client-id-abc:client-secret-xyz",
		},
		{
			name:      "throws when a 200 response carries no access_token",
			status:    http.StatusOK,
			body:      `{"token_type":"Bearer"}`,
			scope:     "read,write",
			wantErr:   "no access_token",
			wantScope: "read,write",
			wantBasic: "client-id-abc:client-secret-xyz",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testMintAppToken(t, test)
		})
	}
}

func testMintAppToken(t *testing.T, test struct {
	name          string
	status        int
	body          string
	scope         string
	wantToken     string
	wantErr       string
	wantScope     string
	wantRespScope string
	wantBasic     string
	wantMethod    string
	wantType      string
	wantExpiry    int
}) {
	t.Helper()
	var got capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = captureRequest(t, r)
		w.WriteHeader(test.status)
		if _, err := io.WriteString(w, test.body); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer server.Close()
	token, err := MintAppToken(
		t.Context(),
		testClient(server),
		Credentials{ClientID: "client-id-abc", ClientSecret: "client-secret-xyz"},
		test.scope,
	)
	assertErrContains(t, err, test.wantErr)
	if token.AccessToken != test.wantToken {
		t.Errorf("access token = %q, want %q", token.AccessToken, test.wantToken)
	}
	if test.wantRespScope != "" && token.Scope != test.wantRespScope {
		t.Errorf("token scope = %q, want %q", token.Scope, test.wantRespScope)
	}
	if test.wantType != "" && token.TokenType != test.wantType {
		t.Errorf("token type = %q, want %q", token.TokenType, test.wantType)
	}
	if test.wantExpiry != 0 && token.ExpiresIn != test.wantExpiry {
		t.Errorf("token expiry = %d, want %d", token.ExpiresIn, test.wantExpiry)
	}
	wantMethod := test.wantMethod
	if wantMethod == "" {
		wantMethod = http.MethodPost
	}
	got.assertForm(t, "/oauth/token", wantMethod, test.wantBasic)
	if got.scope != test.wantScope {
		t.Errorf("scope = %q, want %q", got.scope, test.wantScope)
	}
	if got.grantType != "client_credentials" {
		t.Errorf("grant type = %q, want client_credentials", got.grantType)
	}
}

type capturedRequest struct {
	path, method, contentType, authorization, scope, grantType string
}

func captureRequest(t *testing.T, r *http.Request) capturedRequest {
	t.Helper()
	if err := r.ParseForm(); err != nil {
		t.Errorf("parse form: %v", err)
	}
	return capturedRequest{
		path:          r.URL.Path,
		method:        r.Method,
		contentType:   r.Header.Get("Content-Type"),
		authorization: r.Header.Get("Authorization"),
		scope:         r.Form.Get("scope"),
		grantType:     r.Form.Get("grant_type"),
	}
}

func (got capturedRequest) assertForm(t *testing.T, wantPath, wantMethod, wantBasic string) {
	t.Helper()
	if got.path != wantPath {
		t.Errorf("request path = %q, want %q", got.path, wantPath)
	}
	if got.method != wantMethod {
		t.Errorf("method = %q, want %q", got.method, wantMethod)
	}
	if got.contentType != "application/x-www-form-urlencoded" {
		t.Errorf("content type = %q", got.contentType)
	}
	if !strings.HasPrefix(got.authorization, "Basic ") {
		t.Fatalf("authorization = %q, want Basic auth", got.authorization)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got.authorization, "Basic "))
	if err != nil || string(decoded) != wantBasic {
		t.Errorf("basic credentials = %q, error = %v", decoded, err)
	}
}

func assertErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want containing %q", err, want)
	}
}

func TestRevokeAppToken(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantErr    string
		wantSecret bool
	}{
		{name: "revokes a token with Basic auth and a form body", status: http.StatusOK},
		{name: "revocation errors do not include the token", status: http.StatusBadRequest, wantErr: "400", wantSecret: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var gotMethod, gotContentType, gotAuthorization, gotToken string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotContentType = r.Header.Get("Content-Type")
				gotAuthorization = r.Header.Get("Authorization")
				if err := r.ParseForm(); err != nil {
					t.Errorf("parse form: %v", err)
				}
				gotToken = r.Form.Get("token")
				w.WriteHeader(test.status)
			}))
			defer server.Close()

			err := RevokeAppToken(
				t.Context(),
				testClient(server),
				Credentials{ClientID: "client-id-abc", ClientSecret: "client-secret-xyz"},
				Token{AccessToken: "token-secret"},
			)
			if test.wantErr == "" && err != nil {
				t.Fatalf("RevokeAppToken() error = %v", err)
			}
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("RevokeAppToken() error = %v, want containing %q", err, test.wantErr)
				}
				if strings.Contains(err.Error(), "token-secret") {
					t.Errorf("revocation error exposed token: %v", err)
				}
			}
			if gotMethod != http.MethodPost || gotContentType != "application/x-www-form-urlencoded" {
				t.Errorf("request method/content type = %q/%q", gotMethod, gotContentType)
			}
			wantAuthorization := "Basic " + base64.StdEncoding.EncodeToString([]byte("client-id-abc:client-secret-xyz"))
			if gotAuthorization != wantAuthorization {
				t.Errorf("authorization = %q, want %q", gotAuthorization, wantAuthorization)
			}
			if gotToken != "token-secret" {
				t.Errorf("form token = %q, want token-secret", gotToken)
			}
			if test.wantSecret && err == nil {
				t.Fatal("expected revocation error")
			}
		})
	}
}

// stallTransport blocks until the request context ends, like a hung dial or header wait.
type stallTransport struct{}

func (stallTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	<-request.Context().Done()
	return nil, request.Context().Err()
}

// slowBody returns headers at once and holds the body past the header bound.
type slowBody struct{ wait time.Duration }

func (b slowBody) RoundTrip(*http.Request) (*http.Response, error) {
	reader, writer := io.Pipe()
	go func() {
		<-time.After(b.wait) // The synctest fake clock fires this; it is the delay under test.
		_, _ = writer.Write([]byte(`{"access_token":"tok"}`))
		_ = writer.Close()
	}()
	return &http.Response{StatusCode: http.StatusOK, Body: reader, Header: http.Header{}}, nil
}

func TestPostFormBoundsHeaderWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		_, _, err := postForm(t.Context(), &http.Client{Transport: stallTransport{}}, tokenURL, Credentials{}, url.Values{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("postForm() error = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed != headerWait {
			t.Errorf("gave up after %v, want %v", elapsed, headerWait)
		}
	})
}

func TestPostFormLeavesBodyReadUnbounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		status, body, err := postForm(t.Context(), &http.Client{Transport: slowBody{wait: 2 * headerWait}}, tokenURL, Credentials{}, url.Values{})
		if err != nil || status != http.StatusOK || string(body) != `{"access_token":"tok"}` {
			t.Fatalf("postForm() = %d, %q, %v; want 200 with the full body", status, body, err)
		}
	})
}
