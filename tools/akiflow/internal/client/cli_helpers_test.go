package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/RigelBuild/nebula/tools/akiflow/internal/core"
	"github.com/RigelBuild/nebula/tools/akiflow/internal/keychain"
)

const (
	registerPath = "/oauth/register"
	tokenPath    = "/oauth/token"
	mcpPath      = "/mcp"

	storedTokensJSON  = `{"client_id":"client","access_token":"old-access","refresh_token":"old-refresh","expires_at":100000}`
	expiredTokensJSON = `{"client_id":"client","access_token":"old-access","refresh_token":"old-refresh","expires_at":0}`
)

var challengePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// callRecord is one request the fake Akiflow server received.
type callRecord struct {
	path        string
	contentType string
	auth        string
	protocol    string
	session     string
	id          float64
	method      string
	params      map[string]any
	form        url.Values
}

// recorder collects requests from the httptest server goroutine.
type recorder struct {
	mu    sync.Mutex
	calls []callRecord
}

func (r *recorder) add(c callRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

func (r *recorder) snapshot() []callRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

type stubStore struct {
	value string
	saved string
}

func (s *stubStore) Get(context.Context) (string, error) { return s.value, nil }

func (s *stubStore) Set(_ context.Context, value string) error {
	s.saved = value
	return nil
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return parsed
}

// captureRequest records one request. Form bodies are parsed; JSON bodies yield method, id, and params.
func captureRequest(t *testing.T, rec *recorder, r *http.Request) callRecord {
	t.Helper()
	c := callRecord{
		path:        r.URL.Path,
		contentType: r.Header.Get(core.HeaderContentType),
		auth:        r.Header.Get(core.HeaderAuthorization),
		protocol:    r.Header.Get(core.HeaderProtocolVersion),
		session:     r.Header.Get(core.HeaderSessionID),
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read request body: %v", err)
	}
	switch {
	case strings.HasPrefix(c.contentType, "application/x-www-form-urlencoded"):
		form, parseErr := url.ParseQuery(string(raw))
		if parseErr != nil {
			t.Errorf("parse form body: %v", parseErr)
		}
		c.form = form
	case len(raw) > 0:
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode json body: %v", err)
		}
		c.method, _ = body["method"].(string)
		c.id, _ = body["id"].(float64)
		c.params, _ = body["params"].(map[string]any)
	}
	rec.add(c)
	return c
}

func writeJSONResponse(t *testing.T, w http.ResponseWriter, value any, headers map[string]string) {
	t.Helper()
	for key, val := range headers {
		w.Header().Set(key, val)
	}
	w.Header().Set(core.HeaderContentType, "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("write json response: %v", err)
	}
}

func writeRPC(t *testing.T, w http.ResponseWriter, c callRecord, result any, headers map[string]string) {
	t.Helper()
	writeJSONResponse(t, w, map[string]any{"jsonrpc": "2.0", "id": c.id, "result": result}, headers)
}

func countMethod(calls []callRecord, method string) int {
	count := 0
	for _, c := range calls {
		if c.method == method {
			count++
		}
	}
	return count
}

func methodsOf(calls []callRecord) []string {
	methods := make([]string, 0, len(calls))
	for _, c := range calls {
		methods = append(methods, c.method)
	}
	return methods
}

func pathsOf(calls []callRecord) []string {
	paths := make([]string, 0, len(calls))
	for _, c := range calls {
		paths = append(paths, c.path)
	}
	return paths
}

func outputLines(out *bytes.Buffer) []string {
	text := strings.TrimSuffix(out.String(), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func taskToolList() []any {
	return []any{map[string]any{
		"name": "add-task",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"title": map[string]any{"type": "string"}},
			"required":   []any{"title"},
		},
	}}
}

// newTestApp points every remote endpoint at one httptest server and returns the app wired to store.
func newTestApp(t *testing.T, handler http.Handler, store keychain.CredentialStore, now float64) *Akiflow {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &Akiflow{
		client: server.Client(),
		endpoints: endpoints{
			mcp:      server.URL + mcpPath,
			token:    server.URL + tokenPath,
			register: server.URL + registerPath,
		},
		now:  func() float64 { return now },
		open: func(context.Context, string) (int, error) { return 0, nil },
		serve: func(int, func(*url.URL) keychain.CallbackResult) (func() error, error) {
			return func() error { return nil }, nil
		},
		store: store,
	}
}
