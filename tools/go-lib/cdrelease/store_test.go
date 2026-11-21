package cdrelease

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/kubeserviceaccount"
)

const fakeQueuePath = "/api/v1/namespaces/example-ns/configmaps/cd-release-queue"

// fakeAPI serves ConfigMap example-ns/cd-release-queue the way the API server
// does: a PUT must carry the resourceVersion it read, or it gets a 409. The
// fields below force the failure paths the store must survive.
type fakeAPI struct {
	t         *testing.T
	mu        sync.Mutex
	rv        int
	state     string
	gets      int
	puts      int
	lastPut   []byte
	auth      []string
	conflicts int               // extra 409s to answer before the next PUT is accepted
	fail500   bool              // every PUT answers 500
	omitRV    bool              // GET omits metadata.resourceVersion
	labels    map[string]string // ConfigMap labels the GET serves and the PUT must keep
	forbidden bool              // every PUT answers 403
}

func newFakeAPI(t *testing.T, state string) *fakeAPI {
	t.Helper()
	return &fakeAPI{t: t, rv: 1, state: state}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	if r.URL.Path != fakeQueuePath {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		f.gets++
		f.writeCM(w)
	case http.MethodPut:
		f.puts++
		f.handlePut(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeAPI) handlePut(w http.ResponseWriter, r *http.Request) {
	if f.fail500 {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if f.forbidden {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("read PUT body: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.lastPut = raw
	var cm struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(raw, &cm); err != nil {
		f.t.Errorf("decode PUT body: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if f.conflicts > 0 {
		f.conflicts--
		f.rv++ // another writer won the race
		w.WriteHeader(http.StatusConflict)
		return
	}
	if cm.Metadata.ResourceVersion != strconv.Itoa(f.rv) {
		w.WriteHeader(http.StatusConflict)
		return
	}
	f.state = cm.Data["state"]
	f.rv++
	f.writeCM(w)
}

func (f *fakeAPI) writeCM(w http.ResponseWriter) {
	meta := map[string]any{
		"name":      "cd-release-queue",
		"namespace": "example-ns",
		"annotations": map[string]string{
			"kustomize.toolkit.fluxcd.io/ssa": "IfNotPresent",
		},
	}
	if len(f.labels) > 0 {
		meta["labels"] = f.labels
	}
	if !f.omitRV {
		meta["resourceVersion"] = strconv.Itoa(f.rv)
	}
	raw, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   meta,
		"data":       map[string]string{"state": f.state},
	})
	if err != nil {
		f.t.Errorf("encode ConfigMap: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(raw); err != nil {
		f.t.Errorf("write ConfigMap response: %v", err)
	}
}

func (f *fakeAPI) counts() (gets, puts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets, f.puts
}

func (f *fakeAPI) stored() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeAPI) lastPUT() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPut
}

func (f *fakeAPI) authHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auth...)
}

// connect builds the Cluster the store reads, pointed at the TLS fake. The
// token and namespace come from injected service-account files, and the test
// server's client is the transport, so no real service account is needed.
func connect(t *testing.T, srv *httptest.Server) *kubeserviceaccount.Cluster {
	t.Helper()
	files := map[string]string{
		kubeserviceaccount.TokenPath:     "token-value\n",
		kubeserviceaccount.NamespacePath: "example-ns\n",
		kubeserviceaccount.CAPath:        "unused: the test client trusts the fake",
	}
	cluster, err := kubeserviceaccount.Connect(kubeserviceaccount.Options{
		Env: func(string) string { return "" },
		Read: func(path string) ([]byte, error) {
			v, ok := files[path]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return []byte(v), nil
		},
		HTTP: srv.Client(),
		Base: srv.URL,
	})
	if err != nil {
		t.Fatalf("kubeserviceaccount.Connect: %v", err)
	}
	return cluster
}

// testAttempts is the store's bound on PUT attempts, small enough for a unit test.
const testAttempts = 3

func newTestStore(t *testing.T, srv *httptest.Server) Store {
	t.Helper()
	return newConfigMapStore(connect(t, srv), testAttempts)
}

func TestStoreUpdateAppliesFnAndWritesCAS(t *testing.T) {
	api := newFakeAPI(t, `{"pending":[],"failed":{}}`)
	srv := httptest.NewTLSServer(api)
	t.Cleanup(srv.Close)
	st := newTestStore(t, srv)

	got, err := st.Update(t.Context(), func(s State) (State, error) {
		return Enqueue(s, []string{"aws:up"}), nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !reflect.DeepEqual(got.Pending, []string{"aws:up"}) {
		t.Fatalf("returned Pending = %v", got.Pending)
	}
	if _, puts := api.counts(); puts != 1 {
		t.Fatalf("PUTs = %d, want 1", puts)
	}
	var stored State
	if err := json.Unmarshal([]byte(api.stored()), &stored); err != nil {
		t.Fatalf("stored state is not JSON: %v", err)
	}
	if !reflect.DeepEqual(stored.Pending, []string{"aws:up"}) {
		t.Fatalf("stored Pending = %v", stored.Pending)
	}
	// The PUT replaces the whole object, so it must send back the metadata it
	// read. Dropping the Flux annotations would change how Flux treats the object.
	var put struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(api.lastPUT(), &put); err != nil {
		t.Fatalf("PUT body is not JSON: %v", err)
	}
	if put.Metadata.Annotations["kustomize.toolkit.fluxcd.io/ssa"] != "IfNotPresent" {
		t.Fatalf("PUT dropped the ssa annotation: %v", put.Metadata.Annotations)
	}
}

// The Flux labels on the ConfigMap must survive the PUT, which replaces the whole
// object. A store that rebuilds metadata from a subset strips them on first write.
func TestStoreUpdateKeepsLabelsOnPUT(t *testing.T) {
	api := newFakeAPI(t, `{"pending":[],"failed":{}}`)
	api.labels = map[string]string{
		"kustomize.toolkit.fluxcd.io/name":      "cd-release-queue",
		"kustomize.toolkit.fluxcd.io/namespace": "flux-system",
	}
	srv := httptest.NewTLSServer(api)
	t.Cleanup(srv.Close)
	st := newTestStore(t, srv)

	if _, err := st.Update(t.Context(), func(s State) (State, error) {
		return Enqueue(s, []string{"aws:up"}), nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	var put struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(api.lastPUT(), &put); err != nil {
		t.Fatalf("PUT body is not JSON: %v", err)
	}
	if !reflect.DeepEqual(put.Metadata.Labels, api.labels) {
		t.Fatalf("PUT labels = %v, want the GET's labels %v", put.Metadata.Labels, api.labels)
	}
}

// An fn that returns the state it read must send no PUT: the T3 next and base
// reads and the T8 fenced-out path rely on a read-only Update staying read-only.
func TestStoreUpdateSkipsPUTWhenStateUnchanged(t *testing.T) {
	api := newFakeAPI(t, `{"pending":["a"],"failed":{}}`)
	srv := httptest.NewTLSServer(api)
	t.Cleanup(srv.Close)
	st := newTestStore(t, srv)

	got, err := st.Update(t.Context(), func(s State) (State, error) { return s, nil })
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !reflect.DeepEqual(got.Pending, []string{"a"}) {
		t.Fatalf("returned Pending = %v, want the state read", got.Pending)
	}
	if _, puts := api.counts(); puts != 0 {
		t.Fatalf("PUTs = %d, want 0 when fn returns the input unchanged", puts)
	}
}

// A changed state must still be written: exactly one PUT.
func TestStoreUpdateWritesOncePerChangedState(t *testing.T) {
	api := newFakeAPI(t, `{"pending":["a"],"failed":{}}`)
	srv := httptest.NewTLSServer(api)
	t.Cleanup(srv.Close)
	st := newTestStore(t, srv)

	if _, err := st.Update(t.Context(), func(s State) (State, error) {
		return Enqueue(s, []string{"b"}), nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, puts := api.counts(); puts != 1 {
		t.Fatalf("PUTs = %d, want exactly 1 for a changed state", puts)
	}
}

func TestStoreUpdateRereadsOn409AndSucceeds(t *testing.T) {
	api := newFakeAPI(t, `{"pending":["a"],"failed":{}}`)
	api.conflicts = 1
	srv := httptest.NewTLSServer(api)
	t.Cleanup(srv.Close)
	st := newTestStore(t, srv)

	var seen []State
	got, err := st.Update(t.Context(), func(s State) (State, error) {
		seen = append(seen, s)
		return Enqueue(s, []string{"b"}), nil
	})
	if err != nil {
		t.Fatalf("Update after one 409: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("fn ran %d times, want 2: the 409 forces a re-read and re-apply", len(seen))
	}
	if !reflect.DeepEqual(got.Pending, []string{"a", "b"}) {
		t.Fatalf("Pending = %v, want the re-read state plus the new target", got.Pending)
	}
	gets, puts := api.counts()
	if gets != 2 || puts != 2 {
		t.Fatalf("gets = %d, puts = %d; want one re-read GET and a second PUT", gets, puts)
	}
}

// Retries between attempts wait with a bounded, growing backoff, never zero:
// immediate retries collide again under contention and ride no API blip.
func TestStoreUpdateBacksOffBetweenAttempts(t *testing.T) {
	api := newFakeAPI(t, `{"pending":[],"failed":{}}`)
	api.fail500 = true
	srv := httptest.NewTLSServer(api)
	t.Cleanup(srv.Close)
	st := newConfigMapStore(connect(t, srv), testAttempts).(*configMapStore)
	var waits []time.Duration
	st.sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}

	if _, err := st.Update(t.Context(), func(s State) (State, error) {
		return Enqueue(s, []string{"x"}), nil
	}); err == nil {
		t.Fatal("Update succeeded against a 500; want a loud failure")
	}
	if len(waits) != testAttempts-1 {
		t.Fatalf("waits = %v, want %d backoffs between %d attempts", waits, testAttempts-1, testAttempts)
	}
	for i, d := range waits {
		if d <= 0 || d > writeMaxWait {
			t.Fatalf("wait %d = %v, want within (0, %v]", i, d, writeMaxWait)
		}
	}
	// Update must sleep through the jittered sleeper. An un-jittered wait is
	// exactly the exponential base, so at least one wait must exceed its base.
	jitterSeen := false
	for i, d := range waits {
		if d > writeBaseWait<<i {
			jitterSeen = true
		}
	}
	if !jitterSeen {
		t.Fatalf("waits = %v, all exactly the exponential base: Update does not jitter its backoff", waits)
	}
}

// Jitter spreads colliding retries: the same base wait must land at different
// delays across calls, and every delay stays within [base, min(base*3/2, cap)].
func TestJitteredSleepSpreadsWaitsWithinBound(t *testing.T) {
	const base = 400 * time.Millisecond
	var got []time.Duration
	sleep := jittered(func(_ context.Context, d time.Duration) error {
		got = append(got, d)
		return nil
	})
	for range 64 {
		if err := sleep(t.Context(), base); err != nil {
			t.Fatalf("sleep: %v", err)
		}
	}
	distinct := make(map[time.Duration]bool, len(got))
	for _, d := range got {
		if d < base || d > base+base/2 || d > writeMaxWait {
			t.Fatalf("delay %v outside [%v, min(%v, %v)]", d, base, base+base/2, writeMaxWait)
		}
		distinct[d] = true
	}
	if len(distinct) < 2 {
		t.Fatalf("64 jittered waits produced %d distinct delay(s); want spread", len(distinct))
	}
}

func TestStoreUpdateGivesUpAfterBoundedConflicts(t *testing.T) {
	api := newFakeAPI(t, `{"pending":[],"failed":{}}`)
	api.conflicts = 1000 // every PUT conflicts
	srv := httptest.NewTLSServer(api)
	t.Cleanup(srv.Close)
	st := newTestStore(t, srv)

	_, err := st.Update(t.Context(), func(s State) (State, error) {
		return Enqueue(s, []string{"x"}), nil
	})
	if err == nil {
		t.Fatal("Update succeeded against a store that always conflicts; want a failure")
	}
	if _, puts := api.counts(); puts != testAttempts {
		t.Fatalf("PUTs = %d, want exactly the bound %d", puts, testAttempts)
	}
}

func TestStoreUpdateFailsLoudOn500WithoutFallback(t *testing.T) {
	const before = `{"pending":[],"failed":{}}`
	api := newFakeAPI(t, before)
	api.fail500 = true
	srv := httptest.NewTLSServer(api)
	t.Cleanup(srv.Close)
	st := newTestStore(t, srv)

	_, err := st.Update(t.Context(), func(s State) (State, error) {
		return Enqueue(s, []string{"x"}), nil
	})
	if err == nil {
		t.Fatal("Update returned nil after a 500; want a loud failure")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error %q does not name the status", err)
	}
	if _, puts := api.counts(); puts != testAttempts {
		t.Fatalf("PUTs = %d, want %d bounded attempts", puts, testAttempts)
	}
	if api.stored() != before {
		t.Fatalf("stored state changed to %q despite a failed write", api.stored())
	}
}

// A 403 will not heal on retry, so the store fails on the first PUT.
func TestStoreUpdateFailsFastOnNonConflict4xx(t *testing.T) {
	api := newFakeAPI(t, `{"pending":[],"failed":{}}`)
	api.forbidden = true
	srv := httptest.NewTLSServer(api)
	t.Cleanup(srv.Close)
	st := newTestStore(t, srv)

	if _, err := st.Update(t.Context(), func(s State) (State, error) {
		return Enqueue(s, []string{"x"}), nil
	}); err == nil {
		t.Fatal("Update succeeded against a 403; want a loud failure")
	}
	if _, puts := api.counts(); puts != 1 {
		t.Fatalf("PUTs = %d, want 1: a 403 must not be retried", puts)
	}
}

// Concurrent writers at the production bound must not lose a target silently:
// each writer either lands its target or returns a loud error.
func TestStoreConcurrentWritersLoseNoTargetSilently(t *testing.T) {
	api := newFakeAPI(t, `{"pending":[],"failed":{}}`)
	srv := httptest.NewTLSServer(api)
	t.Cleanup(srv.Close)
	st := newConfigMapStore(connect(t, srv), writeAttempts)

	const writers = 8
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Go(func() {
			target := "t" + strconv.Itoa(i)
			_, errs[i] = st.Update(t.Context(), func(s State) (State, error) {
				return Enqueue(s, []string{target}), nil
			})
		})
	}
	wg.Wait()

	stored := State{}
	if err := json.Unmarshal([]byte(api.stored()), &stored); err != nil {
		t.Fatalf("stored state is not JSON: %v", err)
	}
	landed := make(map[string]bool, len(stored.Pending))
	for _, name := range stored.Pending {
		landed[name] = true
	}
	landedAny := false
	for i := range writers {
		target := "t" + strconv.Itoa(i)
		if errs[i] == nil && !landed[target] {
			t.Fatalf("writer %d returned success but %s is missing from the stored state", i, target)
		}
		if errs[i] == nil {
			landedAny = true
		}
		if errs[i] != nil && !strings.Contains(errs[i].Error(), "after") {
			t.Fatalf("writer %d failed without the bounded-attempts error: %v", i, errs[i])
		}
	}
	// Every writer failing without a write is a broken store, not a loud bound.
	// Serialized CAS retries let at least one writer land.
	if !landedAny {
		t.Fatalf("no writer landed; all %d failed, so the CAS retry never converged", writers)
	}
}

func TestStoreUpdatePropagatesFnError(t *testing.T) {
	api := newFakeAPI(t, `{"pending":[],"failed":{}}`)
	srv := httptest.NewTLSServer(api)
	t.Cleanup(srv.Close)
	st := newTestStore(t, srv)

	sentinel := errors.New("refused")
	// fn runs once. A refusal is permanent: a retry would run fn again with a
	// backoff, so the call count is the check, not only the error.
	calls := 0
	_, err := st.Update(t.Context(), func(s State) (State, error) {
		calls++
		return s, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the fn error returned unchanged", err)
	}
	if calls != 1 {
		t.Fatalf("fn ran %d times, want 1: a refusal from fn must not be retried", calls)
	}
	if _, puts := api.counts(); puts != 0 {
		t.Fatalf("PUTs = %d, want 0 when fn refuses", puts)
	}
}

// A GET that omits resourceVersion would make the PUT unconditional: it would
// overwrite a write it never read. The store must refuse instead.
func TestStoreRefusesToWriteWithoutResourceVersion(t *testing.T) {
	api := newFakeAPI(t, `{"pending":[],"failed":{}}`)
	api.omitRV = true
	srv := httptest.NewTLSServer(api)
	t.Cleanup(srv.Close)
	st := newTestStore(t, srv)

	_, err := st.Update(t.Context(), func(s State) (State, error) {
		return Enqueue(s, []string{"x"}), nil
	})
	if err == nil {
		t.Fatal("Update wrote without a resourceVersion; want an error")
	}
	if _, puts := api.counts(); puts != 0 {
		t.Fatalf("PUTs = %d, want 0: an unconditional PUT can overwrite a concurrent write", puts)
	}
}

func TestStoreSendsServiceAccountToken(t *testing.T) {
	api := newFakeAPI(t, `{"pending":[],"failed":{}}`)
	srv := httptest.NewTLSServer(api)
	t.Cleanup(srv.Close)
	st := newTestStore(t, srv)
	if _, err := st.Update(t.Context(), func(s State) (State, error) { return s, nil }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	auth := api.authHeaders()
	if len(auth) == 0 || auth[0] != "Bearer token-value" {
		t.Fatalf("Authorization = %q, want the trimmed service-account token", auth)
	}
}

func TestNewConfigMapStoreRequiresKubernetesEnv(t *testing.T) {
	_, err := NewConfigMapStore(
		func(string) string { return "" },
		func(string) ([]byte, error) { return []byte("x"), nil },
		&http.Client{},
	)
	if !errors.Is(err, kubeserviceaccount.ErrNotKubernetes) {
		t.Fatalf("err = %v, want kubeserviceaccount.ErrNotKubernetes outside a cluster", err)
	}
}
