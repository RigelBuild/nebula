package mint

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/fakeadmin"
	"github.com/RigelBuild/nebula/tools/garage-bucket-reconcile/internal/garage"
)

var now = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

const (
	adminURL   = "http://garage.example.com:3903"
	masterTok  = "master-token-secret"
	tokenName  = "example-bucket-reconcile"
	secretName = "example-admin-token"
	namespace  = "example-namespace"
)

func testConfig() Config {
	return Config{
		AdminURL:    adminURL,
		MasterToken: masterTok,
		Namespace:   namespace,
		SecretName:  secretName,
		TokenName:   tokenName,
		Scope:       Scope,
		TTL:         TTL,
		RenewBefore: RenewBefore,
	}
}

// fakeSecret is an in-memory Secret that records every replace.
type fakeSecret struct {
	mu      sync.Mutex
	current map[string]any
	writes  []map[string]any
}

func newFakeSecret(initial map[string]any) *fakeSecret {
	return &fakeSecret{current: deepCopy(initial)}
}

func (f *fakeSecret) Read(_ context.Context, _, _ string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return deepCopy(f.current), nil
}

func (f *fakeSecret) Replace(_ context.Context, _, _ string, body map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, deepCopy(body))
	f.current = deepCopy(body)
	return nil
}

func (f *fakeSecret) Writes() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.writes...)
}

// flakySecret fails its first Replace with "publish failed", then delegates to inner.
type flakySecret struct {
	inner *fakeSecret
	mu    sync.Mutex
	calls int
}

func (f *flakySecret) Read(ctx context.Context, ns, name string) (map[string]any, error) {
	return f.inner.Read(ctx, ns, name)
}

func (f *flakySecret) Replace(ctx context.Context, ns, name string, body map[string]any) error {
	f.mu.Lock()
	f.calls++
	first := f.calls == 1
	f.mu.Unlock()
	if first {
		return errors.New("publish failed")
	}
	return f.inner.Replace(ctx, ns, name, body)
}

// conflictSecret reports every Replace as a conflict, as a concurrent writer would.
type conflictSecret struct{ inner *fakeSecret }

func (c conflictSecret) Read(ctx context.Context, ns, name string) (map[string]any, error) {
	return c.inner.Read(ctx, ns, name)
}

func (conflictSecret) Replace(context.Context, string, string, map[string]any) error {
	return ErrSecretConflict
}

// deepCopy round-trips through JSON, so a fake never shares maps with the code under test.
func deepCopy(m map[string]any) map[string]any {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// secret builds a Kubernetes Secret with the given token value and resourceVersion.
func secret(token, resourceVersion string) map[string]any {
	out := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":            secretName,
			"namespace":       namespace,
			"resourceVersion": resourceVersion,
			"annotations":     map[string]any{"kustomize.toolkit.fluxcd.io/ssa": "IfNotPresent"},
		},
		"type": "Opaque",
	}
	if token != "" {
		out["data"] = map[string]any{"token": base64.StdEncoding.EncodeToString([]byte(token))}
	}
	return out
}

// emptySecret is a Secret with no token and resourceVersion "17".
func emptySecret() map[string]any { return secret("", "17") }

// tokenRecord is a GetCurrentAdminTokenInfo or ListAdminTokens record with the given id, scope, expiration and name:
// created is the fake now, and a nil expiration defaults to now plus the mint TTL. A nil id is JSON null.
func tokenRecord(id *string, scope []string, expiration *string, name string) map[string]any {
	exp := isoAfter(TTL)
	if expiration != nil {
		exp = *expiration
	}
	return tokenRecordWith(id, scope, exp, name)
}

// tokenRecordNullExpiry is a token record with a JSON null expiration.
func tokenRecordNullExpiry(id *string, scope []string, name string) map[string]any {
	return tokenRecordWith(id, scope, nil, name)
}

func tokenRecordWith(id *string, scope []string, expiration any, name string) map[string]any {
	out := map[string]any{
		"name":       name,
		"expired":    false,
		"scope":      scope,
		"created":    now.UTC().Format(isoMillis),
		"id":         nil,
		"expiration": expiration,
	}
	if id != nil {
		out["id"] = *id
	}
	return out
}

// isoAfter returns the ISO timestamp d after the fake now, in JavaScript toISOString form.
func isoAfter(d time.Duration) string {
	return now.Add(d).UTC().Format(isoMillis)
}

// createdTokenReply answers CreateAdminToken by echoing the request's scope, expiration and name.
// It returns false when req is not a CreateAdminToken call.
func createdTokenReply(req fakeadmin.Request, id, secretToken string) (fakeadmin.Reply, bool, error) {
	if apiEndpoint(req) != "CreateAdminToken" {
		return fakeadmin.Reply{}, false, nil
	}
	body, ok := req.Body.(map[string]any)
	if !ok {
		return fakeadmin.Reply{}, true, errors.New("CreateAdminToken request did not match the Garage v2 API")
	}
	scope, scopeOK := stringSlice(body["scope"])
	expiration, expOK := body["expiration"].(string)
	name, nameOK := body["name"].(string)
	if !scopeOK || !expOK || !nameOK {
		return fakeadmin.Reply{}, true, errors.New("CreateAdminToken request did not match the Garage v2 API")
	}
	out := tokenRecord(new(id), scope, new(expiration), name)
	out["secretToken"] = secretToken
	return fakeadmin.Reply{Body: out}, true, nil
}

func stringSlice(v any) ([]string, bool) {
	items, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// apiEndpoint returns the last path segment of the request URL, which is the Garage endpoint name.
func apiEndpoint(req fakeadmin.Request) string {
	u, err := url.Parse(req.URL)
	if err != nil {
		return ""
	}
	parts := strings.Split(u.Path, "/")
	return parts[len(parts)-1]
}

func deleteID(req fakeadmin.Request) string {
	u, err := url.Parse(req.URL)
	if err != nil {
		return ""
	}
	return u.Query().Get("id")
}

// healthReady answers /health with 200.
func healthReady(req fakeadmin.Request) (fakeadmin.Reply, bool) {
	if req.URL == adminURL+"/health" {
		return fakeadmin.Reply{Body: "ready"}, true
	}
	return fakeadmin.Reply{}, false
}

// emptyResponder answers health, CreateAdminToken (as "new-id"), ListAdminTokens (with listed) and DeleteAdminToken.
func emptyResponder(listed []any) func(fakeadmin.Request) (fakeadmin.Reply, error) {
	return func(req fakeadmin.Request) (fakeadmin.Reply, error) {
		if r, ok := healthReady(req); ok {
			return r, nil
		}
		if r, ok, err := createdTokenReply(req, "new-id", "new-id.new-secret"); ok {
			return r, err
		}
		switch apiEndpoint(req) {
		case "ListAdminTokens":
			return fakeadmin.Reply{Body: listed}, nil
		case "DeleteAdminToken":
			return fakeadmin.Reply{}, nil
		}
		return fakeadmin.Reply{}, errors.New("unexpected request " + req.URL)
	}
}

// testDeps wires the fake Doer, clock and Secret client into Deps.
func testDeps(d *fakeadmin.Doer, clock *fakeadmin.Clock, secrets SecretClient) Deps {
	return Deps{
		Garage: garage.Deps{
			HTTP:  d.Client(),
			Sleep: clock.Sleep,
			Now:   clock.Now,
			Log:   func(string) {},
		},
		Secrets: secrets,
	}
}

// capturedDeps is testDeps with log lines sent to lines.
func capturedDeps(d *fakeadmin.Doer, clock *fakeadmin.Clock, secrets SecretClient, lines *[]string) Deps {
	dp := testDeps(d, clock, secrets)
	dp.Garage.Log = func(line string) { *lines = append(*lines, line) }
	return dp
}

func fixedClock() *fakeadmin.Clock { return fakeadmin.NewClock(now) }

// mapAt returns m[key] as a map, or nil when it is absent or not an object.
func mapAt(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

func containsEndpoint(reqs []fakeadmin.Request, endpoint string) bool {
	for _, r := range reqs {
		if apiEndpoint(r) == endpoint {
			return true
		}
	}
	return false
}

// jsonEqualAny compares two values through encoding/json, so map key order and numeric types do not matter.
func jsonEqualAny(a, b any) bool {
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ra) == string(rb)
}

// endpointsOf lists each request's Garage endpoint name, in order.
func endpointsOf(reqs []fakeadmin.Request) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, apiEndpoint(r))
	}
	return out
}

// scopeOf returns the scope a CreateAdminToken request asked for.
func scopeOf(req fakeadmin.Request) []string {
	body, _ := req.Body.(map[string]any)
	scope, _ := stringSlice(body["scope"])
	return scope
}

// raceSecret models two overlapping runs. Both reads see the same snapshot. Each publish waits for the other,
// then the winner's succeeds and the loser's gets ErrSecretConflict, as a Kubernetes conflict does.
type raceSecret struct {
	mu         sync.Mutex
	current    map[string]any
	reads      int
	readsReady chan struct{}
	pending    []pendingPublish
	winner     string
}

// pendingPublish is one Replace call waiting for the race to resolve.
type pendingPublish struct {
	body   map[string]any
	result chan error
}

func newRaceSecret(initial map[string]any, winner string) *raceSecret {
	return &raceSecret{current: deepCopy(initial), readsReady: make(chan struct{}), winner: winner}
}

func (r *raceSecret) Read(ctx context.Context, _, _ string) (map[string]any, error) {
	r.mu.Lock()
	snapshot := deepCopy(r.current)
	r.reads++
	if r.reads == 2 {
		close(r.readsReady)
	}
	r.mu.Unlock()
	select {
	case <-r.readsReady:
		return snapshot, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *raceSecret) Replace(ctx context.Context, _, _ string, body map[string]any) error {
	r.mu.Lock()
	result := make(chan error, 1)
	r.pending = append(r.pending, pendingPublish{body: deepCopy(body), result: result})
	if len(r.pending) == 2 {
		for _, p := range r.pending {
			if tokenFromSecret(p.body) == "secret-"+r.winner {
				r.current = deepCopy(p.body)
				p.result <- nil
			} else {
				p.result <- ErrSecretConflict
			}
		}
	}
	r.mu.Unlock()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Writes returns every body that reached Replace, in arrival order.
func (r *raceSecret) Writes() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]map[string]any, 0, len(r.pending))
	for _, p := range r.pending {
		out = append(out, p.body)
	}
	return out
}

func (r *raceSecret) Current() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return deepCopy(r.current)
}
