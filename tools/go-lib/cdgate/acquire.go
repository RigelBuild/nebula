// SPDX-License-Identifier: MIT OR Apache-2.0

package cdgate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/kubeserviceaccount"
)

// Lease timing. The holder renews well inside the 120s duration. A contender
// takes over once a record has not changed for the full duration.
const (
	LeaseDuration     = 120 * time.Second
	LeaseRenewEvery   = 40 * time.Second
	LeaseLossTimeout  = 100 * time.Second
	LeasePollInterval = 5 * time.Second
	LeaseRetryLimit   = 5
)

// Clock is the time source the Lease state machine uses. Wall is for
// timestamps written to the Lease. Monotonic measures elapsed time, so a wall
// clock jump neither hastens nor delays a takeover.
type Clock struct {
	Wall      func() time.Time
	Monotonic func() time.Duration
}

// Sleeper waits d or until ctx is done. It returns ctx.Err() when cancelled.
type Sleeper func(ctx context.Context, d time.Duration) error

// recordKey identifies one observed Lease state. A change in any field
// restarts the contender's takeover clock.
type recordKey struct {
	resourceVersion string
	holder          string
	renewTime       string
}

func keyOf(r *LeaseRecord) recordKey {
	return recordKey{resourceVersion: r.ResourceVersion, holder: r.HolderIdentity, renewTime: r.RenewTime}
}

// leaseWrite builds the body for a create or update. Kubernetes decodes
// acquireTime and renewTime as MicroTime, so timestamps carry six fractional
// digits.
func leaseWrite(name string, record *LeaseRecord, holder string, now time.Time, transitions int64, acquiring bool) LeaseWrite {
	ts := now.UTC().Format("2006-01-02T15:04:05.000000Z")
	w := LeaseWrite{
		Name:                 name,
		HolderIdentity:       holder,
		LeaseDurationSeconds: int64(LeaseDuration / time.Second),
		RenewTime:            ts,
		LeaseTransitions:     transitions,
	}
	if record != nil {
		w.ResourceVersion = record.ResourceVersion
	}
	switch {
	case holder != "" && acquiring:
		w.AcquireTime = ts
	case holder != "" && record != nil && record.AcquireTime != "":
		w.AcquireTime = record.AcquireTime
	case holder != "":
		w.AcquireTime = ts
	case record != nil:
		w.AcquireTime = record.AcquireTime
	}
	return w
}

// AcquireLease waits until this step holds the Lease and returns the monotonic
// time the winning write was sent, which starts the holder's loss deadline. A
// 409 means another writer won, so it re-reads; other failures back off.
func AcquireLease(ctx context.Context, lease *LeaseConfig, clock Clock, sleep Sleeper) (time.Duration, error) {
	var (
		lastKey    recordKey
		haveKey    bool
		observedAt = clock.Monotonic()
		failures   int
	)
	for {
		sentAt, done, err := acquireLeaseIteration(ctx, lease, clock, sleep, &lastKey, &haveKey, &observedAt, &failures)
		if done {
			return sentAt, err
		}
		if err != nil {
			return 0, err
		}
	}
}

func acquireLeaseIteration(ctx context.Context, lease *LeaseConfig, clock Clock, sleep Sleeper, lastKey *recordKey, haveKey *bool, observedAt *time.Duration, failures *int) (time.Duration, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, true, fmt.Errorf("stopped by signal: %w", err)
	}
	record, err := lease.Client.Get(ctx, lease.Name)
	if errors.Is(err, ErrLeaseNotFound) {
		record, err = nil, nil
	}
	if err != nil {
		return 0, false, waitAfterLeaseError(ctx, err, failures, sleep)
	}
	if record == nil {
		return acquireObservedLease(ctx, lease, nil, clock, sleep, failures)
	}
	*lastKey, *observedAt = observeLease(record, clock, *lastKey, *haveKey, *observedAt)
	*haveKey = true
	unchangedFor := clock.Monotonic() - *observedAt
	if record.HolderIdentity == "" || unchangedFor >= LeaseDuration {
		return acquireObservedLease(ctx, lease, record, clock, sleep, failures)
	}
	*failures = 0
	if err := sleep(ctx, min(LeasePollInterval, LeaseDuration-unchangedFor)); err != nil {
		return 0, true, err
	}
	return 0, false, nil
}

func acquireObservedLease(ctx context.Context, lease *LeaseConfig, record *LeaseRecord, clock Clock, sleep Sleeper, failures *int) (time.Duration, bool, error) {
	sentAt, err := writeLease(ctx, lease, record, clock)
	if err == nil {
		return sentAt, true, nil
	}
	return 0, false, waitAfterLeaseError(ctx, err, failures, sleep)
}
func observeLease(record *LeaseRecord, clock Clock, lastKey recordKey, haveKey bool, observedAt time.Duration) (recordKey, time.Duration) {
	key := keyOf(record)
	if haveKey && key == lastKey {
		return lastKey, observedAt
	}
	return key, clock.Monotonic()
}

func writeLease(ctx context.Context, lease *LeaseConfig, record *LeaseRecord, clock Clock) (time.Duration, error) {
	sentAt := clock.Monotonic()
	if record == nil {
		_, err := lease.Client.Create(ctx, leaseWrite(lease.Name, nil, lease.HolderIdentity, clock.Wall(), 1, true))
		return sentAt, err
	}
	_, err := lease.Client.Update(ctx, leaseWrite(lease.Name, record, lease.HolderIdentity, clock.Wall(), record.LeaseTransitions+1, true))
	return sentAt, err
}

func waitAfterLeaseError(ctx context.Context, err error, failures *int, sleep Sleeper) error {
	if kubeserviceaccount.Conflict(err) {
		*failures = 0
		return sleep(ctx, LeasePollInterval)
	}
	*failures++
	if *failures > LeaseRetryLimit {
		return err
	}
	return sleep(ctx, time.Second*(1<<(*failures-1)))
}
