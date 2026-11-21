package cdgate

import (
	"context"
	"errors"
	"time"
)

// ErrNoLongerHeld reports that the Lease names another holder. Renewal stops
// for good, so the caller stops the child.
var ErrNoLongerHeld = errors.New("lease is no longer held by this step")

// LeaseRenewal records how renewal ended. Lost is true when another holder owns
// the Lease, or when no renewal succeeded before the loss deadline.
type LeaseRenewal struct {
	Lost bool
}

// renewOnce reads the Lease and, if this step still holds it, writes a fresh
// renewTime. It returns ErrNoLongerHeld when another holder has the Lease.
func renewOnce(ctx context.Context, lease *LeaseConfig, now time.Time) error {
	record, err := lease.Client.Get(ctx, lease.Name)
	if errors.Is(err, ErrLeaseNotFound) {
		return ErrNoLongerHeld
	}
	if err != nil {
		return err
	}
	if record == nil || record.HolderIdentity != lease.HolderIdentity {
		return ErrNoLongerHeld
	}
	_, err = lease.Client.Update(ctx, leaseWrite(lease.Name, record, lease.HolderIdentity, now, record.LeaseTransitions, false))
	return err
}
func releaseOnce(ctx context.Context, lease *LeaseConfig, clock Clock) error {
	record, err := lease.Client.Get(ctx, lease.Name)
	if errors.Is(err, ErrLeaseNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if record == nil || record.HolderIdentity != lease.HolderIdentity {
		return nil
	}
	_, err = lease.Client.Update(ctx, leaseWrite(lease.Name, record, "", clock.Wall(), record.LeaseTransitions, false))
	return err
}

// Bound returns a context that is cancelled once d has passed. It bounds the
// in-flight renewal at the loss deadline. Production uses WallBound; a test
// supplies one driven by its fake clock, so the abort can be exercised.
type Bound func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc)

// WallBound is the production bound: a real timeout of d.
func WallBound(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

// RenewUntilStopped renews until ctx ends or the Lease is lost. A failed renewal
// is reported and retried on the poll interval, so one bad request does not
// stop the child. bound caps each in-flight renewal at the loss deadline.
func RenewUntilStopped(ctx context.Context, lease *LeaseConfig, acquiredAt time.Duration, clock Clock, sleep Sleeper, bound Bound, onErr func(error)) LeaseRenewal {
	deadline := acquiredAt + LeaseLossTimeout
	delay := LeaseRenewEvery
	for ctx.Err() == nil {
		if clock.Monotonic() >= deadline {
			return LeaseRenewal{Lost: true}
		}
		if err := sleep(ctx, min(delay, deadline-clock.Monotonic())); err != nil {
			return LeaseRenewal{}
		}
		if clock.Monotonic() >= deadline {
			return LeaseRenewal{Lost: true}
		}
		sentAt := clock.Monotonic()
		// Bound the in-flight renewal at the loss deadline, so a hung request
		// cannot delay the stop.
		renewCtx, cancelRenew := bound(ctx, deadline-sentAt)
		err := renewOnce(renewCtx, lease, clock.Wall())
		cancelRenew()
		if err == nil {
			deadline = sentAt + LeaseLossTimeout
			delay = LeaseRenewEvery
			continue
		}
		if errors.Is(err, ErrNoLongerHeld) {
			return LeaseRenewal{Lost: true}
		}
		if ctx.Err() != nil {
			return LeaseRenewal{}
		}
		onErr(err)
		delay = LeasePollInterval
	}
	return LeaseRenewal{}
}

// ReleaseLease clears the holder if this step still holds the Lease. A failure
// is retried with capped backoff. Once LeaseRetryLimit is exceeded the error is
// returned, so a release that never lands fails the step instead of spinning.
// 409s count as failures too.
func ReleaseLease(ctx context.Context, lease *LeaseConfig, clock Clock, sleep Sleeper) error {
	failures := 0
	for {
		err := releaseOnce(ctx, lease, clock)
		if err == nil {
			return nil
		}
		failures++
		if failures > LeaseRetryLimit {
			return err
		}
		if serr := sleep(ctx, time.Second*(1<<(failures-1))); serr != nil {
			return serr
		}
	}
}
