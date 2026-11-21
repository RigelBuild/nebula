package cdgate

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/kubeserviceaccount"
)

// fakeClock is a manual clock. Only the test moves it, through the sleeper,
// so no test waits on the wall clock. It is guarded because the renewal and
// deploy goroutines share it.
type fakeClock struct {
	mu  sync.Mutex
	now time.Duration
	// wallOffset moves only the wall clock, so a test can model a clock jump.
	wallOffset time.Duration
}

// advanceTo moves the monotonic clock forward to at least t. It never moves back,
// so a request that stalls until a deadline cannot rewind time.
func (c *fakeClock) advanceTo(t time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t > c.now {
		c.now = t
	}
}

func (c *fakeClock) monotonic() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) wall() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Unix(1_800_000_000, 0).Add(c.now + c.wallOffset)
}

// sleeper advances the fake clock by d instead of waiting, and records each
// requested wait so a test can assert the poll cadence.
func (c *fakeClock) sleeper(waits *[]time.Duration) Sleeper {
	return func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		*waits = append(*waits, d)
		c.now += d
		return nil
	}
}

// memLease is an in-memory Lease API. Get returns the stored record, and
// Create/Update store the written record with a new resourceVersion.
type memLease struct {
	record     *LeaseRecord
	version    int
	getErr     error
	createErr  error
	updateErrs []error
	writes     []LeaseWrite
}

func (m *memLease) Get(ctx context.Context, _ string) (*LeaseRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.getErr != nil {
		return nil, m.getErr
	}
	if m.record == nil {
		return nil, ErrLeaseNotFound
	}
	cp := *m.record
	return &cp, nil
}

func (m *memLease) Create(ctx context.Context, w LeaseWrite) (*LeaseRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.writes = append(m.writes, w)
	if m.createErr != nil {
		return nil, m.createErr
	}
	m.version++
	m.record = m.store(w, m.version)
	return m.record, nil
}

func (m *memLease) Update(ctx context.Context, w LeaseWrite) (*LeaseRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.writes = append(m.writes, w)
	if len(m.updateErrs) > 0 {
		err := m.updateErrs[0]
		m.updateErrs = m.updateErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	m.version++
	m.record = m.store(w, m.version)
	return m.record, nil
}

func (m *memLease) store(w LeaseWrite, version int) *LeaseRecord {
	return &LeaseRecord{
		ResourceVersion:  fmtVersion(version),
		HolderIdentity:   w.HolderIdentity,
		RenewTime:        w.RenewTime,
		AcquireTime:      w.AcquireTime,
		LeaseTransitions: w.LeaseTransitions,
	}
}

func fmtVersion(v int) string {
	return strconv.Itoa(v)
}

func conflict() error { return &kubeserviceaccount.StatusError{Status: http.StatusConflict} }

func lease(api LeaseClient) *LeaseConfig {
	return &LeaseConfig{Name: "cd-lease", HolderIdentity: "42/step/pod-1", Client: api}
}

func TestAcquireCreatesAbsentLease(t *testing.T) {
	api := &memLease{}
	clk := &fakeClock{}
	var waits []time.Duration
	sentAt, err := AcquireLease(context.Background(), lease(api), Clock{Wall: clk.wall, Monotonic: clk.monotonic}, clk.sleeper(&waits))
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	if len(api.writes) != 1 || api.writes[0].HolderIdentity != "42/step/pod-1" {
		t.Fatalf("writes = %+v, want one create for the step", api.writes)
	}
	if api.writes[0].LeaseTransitions != 1 {
		t.Fatalf("transitions = %d, want 1 on create", api.writes[0].LeaseTransitions)
	}
	if sentAt != 0 || len(waits) != 0 {
		t.Fatalf("sentAt = %v, waits = %v; want immediate create", sentAt, waits)
	}
}

func TestAcquireTakesEmptyHolderWithUpdate(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1", HolderIdentity: "", LeaseTransitions: 4}}
	clk := &fakeClock{}
	var waits []time.Duration
	if _, err := AcquireLease(context.Background(), lease(api), Clock{Wall: clk.wall, Monotonic: clk.monotonic}, clk.sleeper(&waits)); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	if len(api.writes) != 1 {
		t.Fatalf("writes = %d, want one update", len(api.writes))
	}
	w := api.writes[0]
	if w.ResourceVersion != "1" || w.LeaseTransitions != 5 || w.HolderIdentity != "42/step/pod-1" {
		t.Fatalf("update = %+v, want resourceVersion 1, transitions 5, holder set", w)
	}
}

func TestAcquireWaitsWhileHeldThenTakesOverAfterFullDuration(t *testing.T) {
	// The holder never renews, so the record never changes. The contender must
	// wait the full LeaseDuration, measured on its own monotonic clock, then take over.
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1", HolderIdentity: "99/other/pod-9", RenewTime: "t0", LeaseTransitions: 2}}
	clk := &fakeClock{}
	var waits []time.Duration
	sentAt, err := AcquireLease(context.Background(), lease(api), Clock{Wall: clk.wall, Monotonic: clk.monotonic}, clk.sleeper(&waits))
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	var total time.Duration
	for _, w := range waits {
		total += w
	}
	if total < LeaseDuration {
		t.Fatalf("took over after %v, want at least the full %v", total, LeaseDuration)
	}
	if sentAt != clk.now {
		t.Fatalf("sentAt = %v, want the takeover write time %v", sentAt, clk.now)
	}
	if len(api.writes) != 1 || api.writes[0].LeaseTransitions != 3 {
		t.Fatalf("writes = %+v, want one takeover update with transitions 3", api.writes)
	}
	for _, w := range waits {
		if w > LeasePollInterval {
			t.Fatalf("poll wait %v exceeds the poll interval %v", w, LeasePollInterval)
		}
	}
}

func TestAcquireRecordChangeRestartsCountFromOwnClock(t *testing.T) {
	// The record changes after 60s. The takeover clock must restart then, so
	// the contender takes over LeaseDuration after the change, not after first sight.
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1", HolderIdentity: "99/other", RenewTime: "t0"}}
	clk := &fakeClock{}
	var waits []time.Duration
	sleep := func(ctx context.Context, d time.Duration) error {
		if err := clk.sleeper(&waits)(ctx, d); err != nil {
			return err
		}
		if clk.now >= 60*time.Second && api.record.RenewTime == "t0" {
			api.record = &LeaseRecord{ResourceVersion: "2", HolderIdentity: "99/other", RenewTime: "t1"}
		}
		return nil
	}
	sentAt, err := AcquireLease(context.Background(), lease(api), Clock{Wall: clk.wall, Monotonic: clk.monotonic}, sleep)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	if sentAt-60*time.Second < LeaseDuration {
		t.Fatalf("took over %v after the change, want at least %v", sentAt-60*time.Second, LeaseDuration)
	}
}

func TestAcquireTreats409AsLostRaceAndKeepsWaiting(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1", HolderIdentity: ""}}
	api.updateErrs = []error{conflict(), conflict(), nil}
	clk := &fakeClock{}
	var waits []time.Duration
	if _, err := AcquireLease(context.Background(), lease(api), Clock{Wall: clk.wall, Monotonic: clk.monotonic}, clk.sleeper(&waits)); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	if len(api.writes) != 3 {
		t.Fatalf("writes = %d, want 3 attempts through two conflicts", len(api.writes))
	}
}

type spacedFailureLease struct {
	*memLease
	remaining int
	reads     int
}

func (s *spacedFailureLease) Get(ctx context.Context, name string) (*LeaseRecord, error) {
	if s.remaining == 0 {
		return s.memLease.Get(ctx, name)
	}
	s.reads++
	if s.reads%2 == 1 {
		s.remaining--
		return nil, errors.New("apiserver unavailable")
	}
	return &LeaseRecord{ResourceVersion: "1", HolderIdentity: "99/other"}, nil
}

func TestAcquireSuccessfulHeldPollResetsFailures(t *testing.T) {
	api := &spacedFailureLease{
		memLease:  &memLease{record: &LeaseRecord{ResourceVersion: "1"}},
		remaining: LeaseRetryLimit + 1,
	}
	clk := &fakeClock{}
	var waits []time.Duration
	if _, err := AcquireLease(context.Background(), lease(api), Clock{Wall: clk.wall, Monotonic: clk.monotonic}, clk.sleeper(&waits)); err != nil {
		t.Fatalf("AcquireLease after spaced Get errors: %v", err)
	}
	if api.remaining != 0 || len(api.writes) != 1 || api.writes[0].HolderIdentity != lease(api).HolderIdentity {
		t.Fatalf("remaining errors = %d, writes = %+v; want all spaced errors survived and a Lease acquired", api.remaining, api.writes)
	}
}

func TestAcquireRetriesNonConflictWithBackoffThenFails(t *testing.T) {
	boom := errors.New("apiserver unavailable")
	api := &memLease{getErr: boom}
	clk := &fakeClock{}
	var waits []time.Duration
	_, err := AcquireLease(context.Background(), lease(api), Clock{Wall: clk.wall, Monotonic: clk.monotonic}, clk.sleeper(&waits))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the final apiserver error", err)
	}
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("backoff waits = %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("backoff waits = %v, want %v", waits, want)
		}
	}
}

func TestAcquireRetriesWriteFailureWithBackoff(t *testing.T) {
	boom := errors.New("apiserver unavailable")
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1", HolderIdentity: ""}}
	api.updateErrs = []error{boom, nil}
	clk := &fakeClock{}
	var waits []time.Duration
	if _, err := AcquireLease(context.Background(), lease(api), Clock{Wall: clk.wall, Monotonic: clk.monotonic}, clk.sleeper(&waits)); err != nil {
		t.Fatalf("AcquireLease: %v, want a write failure retried to success", err)
	}
	if len(api.writes) != 2 {
		t.Fatalf("writes = %d, want 2 attempts through one failure", len(api.writes))
	}
	if len(waits) != 1 || waits[0] != time.Second {
		t.Fatalf("backoff waits = %v, want one 1s backoff before the retry", waits)
	}
}

func TestAcquireWrite409WaitsPollInterval(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1", HolderIdentity: ""}}
	api.updateErrs = []error{conflict(), nil}
	clk := &fakeClock{}
	var waits []time.Duration
	if _, err := AcquireLease(context.Background(), lease(api), Clock{Wall: clk.wall, Monotonic: clk.monotonic}, clk.sleeper(&waits)); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	if len(waits) == 0 || waits[0] != LeasePollInterval {
		t.Fatalf("waits = %v, want the first wait after a write 409 to be LeasePollInterval", waits)
	}
}

func TestAcquireStopsWhenContextCancelled(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1", HolderIdentity: "99/other"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	clk := &fakeClock{}
	var waits []time.Duration
	_, err := AcquireLease(ctx, lease(api), Clock{Wall: clk.wall, Monotonic: clk.monotonic}, clk.sleeper(&waits))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(api.writes) != 0 {
		t.Fatal("a cancelled acquire still wrote the Lease")
	}
}
