package cdgate

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/kubeserviceaccount"
)

// sharedLease is one Lease that several contenders write concurrently. Every
// write carries the resourceVersion it read, and a stale one gets a 409, as the
// Kubernetes API does. The mutex makes each read and write atomic.
type sharedLease struct {
	mu        sync.Mutex
	record    *LeaseRecord
	version   int
	conflicts int
	// reads, when set, is a barrier: each first Get waits until both contenders
	// have read, so both writes race for the same resourceVersion.
	reads     *sync.WaitGroup
	readCount int
}

func (s *sharedLease) Get(context.Context, string) (*LeaseRecord, error) {
	s.mu.Lock()
	first := s.readCount < 2 && s.reads != nil
	if first {
		s.readCount++
	}
	var snapshot *LeaseRecord
	if s.record != nil {
		cp := *s.record
		snapshot = &cp
	}
	s.mu.Unlock()
	if first {
		// Both contenders have read the same state before either writes.
		s.reads.Done()
		s.reads.Wait()
	}
	if snapshot == nil {
		return nil, ErrLeaseNotFound
	}
	return snapshot, nil
}

func (s *sharedLease) Create(_ context.Context, w LeaseWrite) (*LeaseRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record != nil {
		s.conflicts++
		return nil, conflict()
	}
	s.version++
	s.record = &LeaseRecord{ResourceVersion: fmtVersion(s.version), HolderIdentity: w.HolderIdentity, RenewTime: w.RenewTime, AcquireTime: w.AcquireTime, LeaseTransitions: w.LeaseTransitions}
	cp := *s.record
	return &cp, nil
}

func (s *sharedLease) Update(_ context.Context, w LeaseWrite) (*LeaseRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(w); err != nil {
		return nil, err
	}
	s.version++
	s.record = &LeaseRecord{ResourceVersion: fmtVersion(s.version), HolderIdentity: w.HolderIdentity, RenewTime: w.RenewTime, AcquireTime: w.AcquireTime, LeaseTransitions: w.LeaseTransitions}
	cp := *s.record
	return &cp, nil
}

// check rejects a write whose resourceVersion is not the current one. It must
// be called with the mutex held.
func (s *sharedLease) check(w LeaseWrite) error {
	if s.record == nil {
		return nil
	}
	if w.ResourceVersion != s.record.ResourceVersion {
		s.conflicts++
		return conflict()
	}
	return nil
}

// taggedLease is one contender's view of a sharedLease. Its Get fires onPoll
// only after this contender's own write was refused with a 409: that Get is
// the loser polling the Lease it lost. A read before any refused write (the
// pre-race read) never fires it.
type taggedLease struct {
	api     *sharedLease
	refused *atomic.Bool
	onPoll  func()
}

func (t taggedLease) Get(ctx context.Context, name string) (*LeaseRecord, error) {
	rec, err := t.api.Get(ctx, name)
	if t.refused.Load() {
		t.onPoll()
	}
	return rec, err
}

func (t taggedLease) Create(ctx context.Context, w LeaseWrite) (*LeaseRecord, error) {
	rec, err := t.api.Create(ctx, w)
	if kubeserviceaccount.Conflict(err) {
		t.refused.Store(true)
	}
	return rec, err
}

func (t taggedLease) Update(ctx context.Context, w LeaseWrite) (*LeaseRecord, error) {
	rec, err := t.api.Update(ctx, w)
	if kubeserviceaccount.Conflict(err) {
		t.refused.Store(true)
	}
	return rec, err
}

// TestTwoContendersNeverDeployAtOnce mirrors the TS test of the same name. Two
// contenders read the same free Lease before either writes, so both race for
// one resourceVersion. The winner holds the deploy until the loser has polled.
// Both exit 0, the loser sees a 409, and no two deploys overlap.
func TestTwoContendersNeverDeployAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var reads sync.WaitGroup
		reads.Add(2)
		api := &sharedLease{reads: &reads}
		loserPolled := make(chan struct{})
		var pollOnce sync.Once
		// The winner holds the deploy until a contender polls the Lease after its
		// own refused write: that poll is the loser waiting on the winner.
		tagged := func() LeaseClient {
			refused := &atomic.Bool{}
			return taggedLease{api: api, refused: refused, onPoll: func() { pollOnce.Do(func() { close(loserPolled) }) }}
		}
		var mu sync.Mutex
		active, maxActive := 0, 0
		var deployed []string
		exec := func(holder string) Executor {
			return func(ctx context.Context, _ bool) (StepResult, error) {
				mu.Lock()
				active++
				maxActive = max(maxActive, active)
				deployed = append(deployed, holder)
				first := len(deployed) == 1
				mu.Unlock()
				defer func() {
					mu.Lock()
					active--
					mu.Unlock()
				}()
				if first {
					// Bounded on fake time: a loser that never polls after its 409
					// must fail here by name, not run to the go test timeout.
					select {
					case <-loserPolled:
					case <-time.After(10 * time.Minute):
						t.Error("the loser never polled the Lease after its 409")
						return StepResult{}, errors.New("winner wait timed out")
					case <-ctx.Done():
						return StepResult{}, ctx.Err()
					}
				}
				return StepResult{Code: 0}, nil
			}
		}
		var wg sync.WaitGroup
		start := time.Now()
		clock := Clock{
			Wall:      time.Now,
			Monotonic: func() time.Duration { return time.Since(start) },
		}
		codes := make([]int, 2)
		for i, holder := range []string{"a/pod", "b/pod"} {
			wg.Go(func() {
				codes[i] = RunCD(context.Background(), "nix:deploy", Run{
					Gate:  func(context.Context) (Verdict, error) { return Verdict{Deploy: true}, nil },
					Exec:  exec(holder),
					Clock: clock,
					Sleep: sleepOrDone,
					Lease: &LeaseConfig{Name: "cd-lease", HolderIdentity: holder, Client: tagged()},
				})
			})
		}
		wg.Wait()
		if codes[0] != 0 || codes[1] != 0 {
			t.Fatalf("codes = %v, want [0 0]: both contenders must deploy in turn", codes)
		}
		api.mu.Lock()
		conflicts := api.conflicts
		api.mu.Unlock()
		if conflicts == 0 {
			t.Fatal("no 409 was issued: the contenders did not race for the Lease")
		}
		if maxActive != 1 {
			t.Fatalf("maxActive = %d, want 1: the deploy ran beside another", maxActive)
		}
		slices.Sort(deployed)
		if !slices.Equal(deployed, []string{"a/pod", "b/pod"}) {
			t.Fatalf("deployed = %v, want both holders", deployed)
		}
	})
}

// sleepOrDone waits d on the bubble's timer or returns when ctx is done.
func sleepOrDone(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
