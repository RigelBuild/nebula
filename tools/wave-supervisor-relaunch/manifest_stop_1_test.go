package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestManifestStopWave(t *testing.T) {
	t.Run("stops then polls presence until clear", testManifestStopWaveStopsThenPollsPresenceUntilClear)

	t.Run("stops managed seats before the slice and ignores seat-stop failure", testManifestStopWaveStopsManagedSeatsBeforeTheSlice)

	t.Run("a hanging seat stop is bounded before the slice stop", testManifestStopWaveAHangingSeatStopIsBounded)

	t.Run("reports stop failure and stuck presence", testManifestStopWaveReportsStopFailureAndStuckPresence)
}

func TestSeatStopDeadlines(t *testing.T) {
	t.Run("stopWave outlasts the runner, so the runner kills hung stops before the blocking slice stop", func(t *testing.T) {
		if seatStopOuterDeadlineMS <= seatStopDeadlineMS {
			t.Fatalf("seat stop outer deadline = %dms, runner deadline = %dms", seatStopOuterDeadlineMS, seatStopDeadlineMS)
		}
	})
}

func testManifestStopWaveStopsThenPollsPresenceUntilClear(t *testing.T) {
	t.Helper()
	polls := 0
	result := stopWave(t.Context(), stopWaveDeps{
		CheckSubagents: func(context.Context) error { return nil },
		StopSlice:      func(context.Context) (bool, error) { return true, nil },
		PresenceGone: func(context.Context) (bool, error) {
			polls++
			return polls == 3, nil
		},
		Now:      func() time.Time { return time.Unix(0, 0) },
		Sleep:    func(context.Context, time.Duration) error { return nil },
		Deadline: 30 * time.Second,
		Poll:     2 * time.Second,
	})
	if result != "stopped" || polls != 3 {
		t.Fatalf("stopWave() = %q after %d polls, want stopped after 3", result, polls)
	}
}

func testManifestStopWaveStopsManagedSeatsBeforeTheSlice(t *testing.T) {
	t.Helper()
	var events []string
	result := stopWave(t.Context(), stopWaveDeps{
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
		OnWarning:    func(string, error) {},
	})
	if result != "stopped" {
		t.Fatalf("stopWave() = %q, want stopped", result)
	}
	if !slices.Equal(events, []string{"seats", "slice"}) {
		t.Fatalf("stop events = %#v, want [seats slice]", events)
	}
}

func testManifestStopWaveAHangingSeatStopIsBounded(t *testing.T) {
	t.Helper()
	testCtx := t.Context()
	var timeout *fakeTimeoutContext
	deadlineReady := make(chan time.Duration, 1)
	seatStarted := make(chan struct{})
	seatStopped := make(chan struct{})
	events := make(chan string, 3)
	runStop := make(chan string, 1)
	go func() {
		runStop <- stopWave(testCtx, stopWaveDeps{
			CheckSubagents: func(context.Context) error { return nil },
			StopSeats: func(ctx context.Context) error {
				events <- "seats"
				close(seatStarted)
				defer close(seatStopped)
				<-ctx.Done()
				return ctx.Err()
			},
			StopSeatsDeadline: 5 * time.Millisecond,
			WithTimeout: func(parent context.Context, deadline time.Duration) (context.Context, context.CancelFunc) {
				timeout = newFakeTimeoutContext(parent)
				deadlineReady <- deadline
				return timeout, func() { timeout.finish(context.Canceled) }
			},
			StopSlice: func(context.Context) (bool, error) {
				events <- "slice"
				return true, nil
			},
			PresenceGone: func(context.Context) (bool, error) {
				events <- "presence"
				return true, nil
			},
			OnWarning: func(string, error) {},
		})
	}()

	if got := <-deadlineReady; got != 5*time.Millisecond {
		t.Fatalf("seat stop deadline = %s, want 5ms", got)
	}
	<-seatStarted
	if event := <-events; event != "seats" {
		t.Fatalf("event before fake deadline = %q, want seats", event)
	}
	if timeout == nil {
		t.Fatal("seat stop did not initialize the injected timeout")
	}
	timeout.finish(context.DeadlineExceeded)
	if got := <-runStop; got != "stopped" {
		t.Fatalf("stopWave() = %q, want stopped", got)
	}
	<-seatStopped
	gotEvents := []string{<-events, <-events}
	if !slices.Equal(gotEvents, []string{"slice", "presence"}) {
		t.Fatalf("events after bounded seat stop = %#v, want [slice presence]", gotEvents)
	}
}

func testManifestStopWaveReportsStopFailureAndStuckPresence(t *testing.T) {
	t.Helper()
	result := stopWave(t.Context(), stopWaveDeps{
		CheckSubagents: func(context.Context) error { return nil },
		StopSlice:      func(context.Context) (bool, error) { return false, nil },
		PresenceGone:   func(context.Context) (bool, error) { return true, nil },
		Now:            func() time.Time { return time.Unix(0, 0) },
		Sleep:          func(context.Context, time.Duration) error { return nil },
		Deadline:       30 * time.Second,
		Poll:           2 * time.Second,
	})
	if result != "wave-stop" {
		t.Fatalf("stopWave() after failed slice stop = %q, want wave-stop", result)
	}

	now := time.Unix(0, 0)
	var delays []time.Duration
	result = stopWave(t.Context(), stopWaveDeps{
		CheckSubagents: func(context.Context) error { return nil },
		StopSlice:      func(context.Context) (bool, error) { return true, nil },
		PresenceGone:   func(context.Context) (bool, error) { return false, nil },
		Now:            func() time.Time { return now },
		Sleep: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			now = now.Add(delay)
			return nil
		},
		Deadline: 3 * time.Second,
		Poll:     2 * time.Second,
	})
	if result != "presence-stuck" {
		t.Fatalf("stopWave() with persistent presence = %q, want presence-stuck", result)
	}
	if !slices.Equal(delays, []time.Duration{2 * time.Second, time.Second}) {
		t.Fatalf("presence poll delays = %#v, want [2s 1s]", delays)
	}
}
