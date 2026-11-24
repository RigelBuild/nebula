package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStopWave(t *testing.T) {

	t.Run("stops then polls presence until clear", testStopWaveStopsThenPollsPresenceUntilClear)

	t.Run("stops managed seats before the slice and ignores seat failure", testStopWaveStopsManagedSeatsBeforeTheSlice)

	t.Run("a hanging fake seat stop is bounded before slice stop", testStopWaveAHangingFakeSeatStopIs)

	t.Run("reports slice failure and stuck presence", testStopWaveReportsSliceFailureAndStuckPresence)
	t.Run("uses default stop poll interval and deadline through injected clock", testStopWaveUsesDefaultStopPollIntervalAnd)
	t.Run("treats caller cancellation while polling as stuck presence", testStopWaveTreatsCallerCancellationWhilePollingAs)
}

func testStopWaveStopsThenPollsPresenceUntilClear(t *testing.T) {
	t.Helper()
	now := time.Unix(0, 0)
	polls := 0
	got := stopWave(context.Background(), stopWaveDeps{
		CheckSubagents: func(context.Context) error { return nil },
		StopSlice:      func(context.Context) (bool, error) { return true, nil },
		PresenceGone: func(context.Context) (bool, error) {
			polls++
			return polls == 3, nil
		},
		Now: func() time.Time { return now },
		Sleep: func(_ context.Context, delay time.Duration) error {
			now = now.Add(delay)
			return nil
		},
		Deadline: 30 * time.Second,
		Poll:     2 * time.Second,
	})
	if got != "stopped" || polls != 3 {
		t.Fatalf("stopWave() = %q after %d polls", got, polls)
	}
}

func testStopWaveStopsManagedSeatsBeforeTheSlice(t *testing.T) {
	t.Helper()
	events := []string{}
	got := stopWave(context.Background(), stopWaveDeps{
		CheckSubagents: func(context.Context) error { return nil },
		StopSeats: func(context.Context) error {
			events = append(events, "seats")
			return errors.New("seat stop failed")
		},
		StopSlice: func(context.Context) (bool, error) {
			events = append(events, "slice")
			return true, nil
		},
		PresenceGone: func(context.Context) (bool, error) { return true, nil },
	})
	if got != "stopped" || strings.Join(events, ",") != "seats,slice" {
		t.Fatalf("stopWave() = %q, events=%v", got, events)
	}
}

func testStopWaveAHangingFakeSeatStopIs(t *testing.T) {
	t.Helper()
	seatStarted := make(chan struct{})
	seatDone := make(chan struct{})
	sliceStopped := false
	var requestedDeadline time.Duration
	got := stopWave(context.Background(), stopWaveDeps{
		CheckSubagents: func(context.Context) error { return nil },
		StopSeats: func(ctx context.Context) error {
			close(seatStarted)
			<-ctx.Done()
			close(seatDone)
			return ctx.Err()
		},
		StopSeatsDeadline: 5 * time.Millisecond,
		WithTimeout: func(ctx context.Context, deadline time.Duration) (context.Context, context.CancelFunc) {
			requestedDeadline = deadline
			return context.WithTimeout(ctx, deadline)
		},
		StopSlice: func(context.Context) (bool, error) {
			<-seatStarted
			sliceStopped = true
			return true, nil
		},
		PresenceGone: func(context.Context) (bool, error) { return true, nil },
	})
	<-seatDone
	if got != "stopped" || !sliceStopped || requestedDeadline != 5*time.Millisecond {
		t.Fatalf("stopWave() = %q, sliceStopped=%t, deadline=%s", got, sliceStopped, requestedDeadline)
	}
}

func testStopWaveReportsSliceFailureAndStuckPresence(t *testing.T) {
	t.Helper()
	if got := stopWave(context.Background(), stopWaveDeps{
		CheckSubagents: func(context.Context) error { return nil },
		StopSlice:      func(context.Context) (bool, error) { return false, nil },
		PresenceGone:   func(context.Context) (bool, error) { return true, nil },
	}); got != "wave-stop" {
		t.Fatalf("failed stopWave() = %q, want wave-stop", got)
	}
	now := time.Unix(0, 0)
	if got := stopWave(context.Background(), stopWaveDeps{
		CheckSubagents: func(context.Context) error { return nil },
		StopSlice:      func(context.Context) (bool, error) { return true, nil },
		PresenceGone:   func(context.Context) (bool, error) { return false, nil },
		Now:            func() time.Time { return now },
		Sleep: func(_ context.Context, delay time.Duration) error {
			now = now.Add(delay)
			return nil
		},
		Deadline: 3 * time.Second,
		Poll:     2 * time.Second,
	}); got != "presence-stuck" {
		t.Fatalf("stuck stopWave() = %q, want presence-stuck", got)
	}
}

func testStopWaveUsesDefaultStopPollIntervalAnd(t *testing.T) {
	t.Helper()
	now := time.Unix(0, 0)
	var delays []time.Duration
	got := stopWave(context.Background(), stopWaveDeps{
		CheckSubagents: func(context.Context) error { return nil },
		StopSlice:      func(context.Context) (bool, error) { return true, nil },
		PresenceGone:   func(context.Context) (bool, error) { return false, nil },
		Now:            func() time.Time { return now },
		Sleep: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			now = now.Add(delay)
			return nil
		},
	})
	if got != "presence-stuck" || len(delays) != 15 || delays[0] != 2*time.Second || delays[len(delays)-1] != 2*time.Second {
		t.Fatalf("stopWave()=%q delays=%v", got, delays)
	}
}

func testStopWaveTreatsCallerCancellationWhilePollingAs(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := stopWave(ctx, stopWaveDeps{
		CheckSubagents: func(context.Context) error { return nil },
		StopSlice:      func(context.Context) (bool, error) { return true, nil },
		PresenceGone:   func(context.Context) (bool, error) { return false, nil },
		Sleep: func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		},
	})
	if got != "presence-stuck" {
		t.Fatalf("stopWave() = %q, want presence-stuck", got)
	}
}
