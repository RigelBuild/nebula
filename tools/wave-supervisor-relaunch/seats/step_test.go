package seats

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func foreman(gibs int64) seat { return idleSeat("foreman", gibs) }

func assertCalls(t *testing.T, w *world, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if w.calls == nil {
		w.calls = []string{}
	}
	if !slices.Equal(w.calls, want) {
		t.Fatalf("commands = %q, want %q (logs %q)", w.calls, want, w.logs)
	}
}

func assertLogs(t *testing.T, w *world, want ...string) {
	t.Helper()
	if !slices.Equal(w.logs, want) {
		t.Fatalf("logs = %q, want %q", w.logs, want)
	}
}

func TestStepRelaunchesIdleSeatOverThreshold(t *testing.T) {
	w := newWorld(foreman(1), idleSeat("accounts", 10))
	w.run(t.Context())
	assertCalls(t, w, "cotal stop --name accounts --space main", "wave-seats restart accounts")
	assertLogs(t, w, outcomeLine("relaunched", "accounts", 10))
}

func TestStepSkipsWorkingSeat(t *testing.T) {
	w := newWorld(foreman(1), workingSeat("accounts", 10))
	w.run(t.Context())
	assertCalls(t, w)
	assertLogs(t, w, outcomeLine("skipped:working", "accounts", 10))
}

func TestStepSkipsSeatUnderThreshold(t *testing.T) {
	w := newWorld(foreman(1), idleSeat("accounts", 4))
	w.run(t.Context())
	assertCalls(t, w)
	assertLogs(t, w)
}

func TestStepNeverRelaunchesRoot(t *testing.T) {
	w := newWorld(foreman(12), idleSeat("accounts", 4))
	w.run(t.Context())
	assertCalls(t, w)
	for _, line := range w.logs {
		if strings.Contains(line, "seat=foreman") {
			t.Fatalf("root seat logged: %q", line)
		}
	}
}

func TestStepRootInactiveDoesNothing(t *testing.T) {
	w := newWorld(idleSeat("accounts", 10))
	w.run(t.Context())
	assertCalls(t, w)
	assertLogs(t, w, "wave-supervisor-relaunch: seat outcome=skipped reason=root-seat-inactive")
}

func TestStepHoldSkipsEverySeat(t *testing.T) {
	w := newWorld(foreman(1), idleSeat("accounts", 10), idleSeat("supervisor", 11))
	w.hold = true
	w.run(t.Context())
	assertCalls(t, w)
	assertLogs(t, w, outcomeLine("skipped:hold", "supervisor", 11), outcomeLine("skipped:hold", "accounts", 10))
}

func TestStepOnePerRunLargestFirst(t *testing.T) {
	w := newWorld(foreman(1), idleSeat("accounts", 10), idleSeat("supervisor", 12))
	w.run(t.Context())
	assertCalls(t, w, "cotal stop --name supervisor --space main", "wave-seats restart supervisor")
	assertLogs(t, w, outcomeLine("relaunched", "supervisor", 12), outcomeLine("skipped:one-per-run", "accounts", 10))
}

func TestStepFailedStopNeverRestarts(t *testing.T) {
	for name, configure := range map[string]func(*world){
		"non-zero exit": func(w *world) { w.stopCode = 1 },
		"timeout":       func(w *world) { w.stopErr = context.DeadlineExceeded },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(foreman(1), idleSeat("supervisor", 12), idleSeat("accounts", 10))
			configure(w)
			w.run(t.Context())
			assertCalls(t, w, "cotal stop --name supervisor --space main")
			if len(w.logs) != 2 || !strings.Contains(w.logs[0], "outcome=failed seat=supervisor") || w.logs[1] != outcomeLine("skipped:one-per-run", "accounts", 10) {
				t.Fatalf("logs = %q", w.logs)
			}
		})
	}
}

func TestStepIdleRecheckBeforeStop(t *testing.T) {
	// The roster turns working on the re-check, so no command runs and the next seat is tried.
	w := newWorld(foreman(1), idleSeat("supervisor", 12), idleSeat("accounts", 10))
	w.busyAfter = "supervisor"
	w.run(t.Context())
	assertCalls(t, w, "cotal stop --name accounts --space main", "wave-seats restart accounts")
	assertLogs(t, w, outcomeLine("skipped:working", "supervisor", 12), outcomeLine("relaunched", "accounts", 10))
}

func TestStepBackgroundRecheckBeforeStop(t *testing.T) {
	// Background turns running on the re-check, so no command runs and the next seat is tried.
	w := newWorld(foreman(1), idleSeat("supervisor", 12), idleSeat("accounts", 10))
	w.backgroundAfter = "supervisor"
	w.run(t.Context())
	assertCalls(t, w, "cotal stop --name accounts --space main", "wave-seats restart accounts")
	assertLogs(t, w, outcomeLine("skipped:working", "supervisor", 12), outcomeLine("relaunched", "accounts", 10))
}

func TestStepRecheckReadFailureNeverStops(t *testing.T) {
	w := newWorld(foreman(1), idleSeat("accounts", 10))
	w.psOverride = func(call int) string {
		if call > 1 {
			return "not json"
		}
		return w.roster()
	}
	w.run(t.Context())
	assertCalls(t, w)
	if len(w.logs) != 1 || !strings.Contains(w.logs[0], "outcome=failed seat=accounts") {
		t.Fatalf("logs = %q", w.logs)
	}
}

func TestStepWaitsForAbsenceBeforeRestart(t *testing.T) {
	w := newWorld(foreman(1), idleSeat("accounts", 10))
	w.present = func(call int) (string, string, any) {
		if call < 2 {
			return psEnvelope(`{"name":"accounts"}`), "", readyStatus()
		}
		return psEnvelope(""), "", readyStatus()
	}
	w.run(t.Context())
	assertCalls(t, w, "cotal stop --name accounts --space main", "wave-seats restart accounts")
	if w.presenceN != 3 {
		t.Fatalf("presence probes = %d, want 3", w.presenceN)
	}
}

// TestStepPresenceMatchesRestart pins the presence test to wave-seats restart's: a running holder
// or a retrying manager failure still counts as present, so restart is never sent and refused.
func TestStepPresenceMatchesRestart(t *testing.T) {
	retrying := readyStatus()
	retrying["failures"] = []any{map[string]any{"alias": "accounts", "phase": "retrying"}}
	for name, present := range map[string]func(int) (string, string, any){
		"retrying failure": func(int) (string, string, any) { return psEnvelope(""), "", retrying },
		"running holder": func(int) (string, string, any) {
			return psEnvelope(""), `{"name":"accounts","status":"running"}`, readyStatus()
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(foreman(1), idleSeat("accounts", 10))
			w.present = present
			w.run(t.Context())
			assertCalls(t, w, "cotal stop --name accounts --space main")
			if len(w.logs) != 1 || !strings.Contains(w.logs[0], "still present after stop") {
				t.Fatalf("logs = %q", w.logs)
			}
		})
	}
}

// TestStepFailedReadAfterStopNeverRestarts: after stop, a failed holder or ps read, or a non-JSON
// ps envelope, leaves liveness unknown, so the wait holds to the deadline and restart is never sent.
func TestStepFailedReadAfterStopNeverRestarts(t *testing.T) {
	for name, tc := range map[string]struct {
		configure func(*world)
		reason    string
	}{
		"holder read error": {func(w *world) { w.holderErrAfter = errors.New("holder socket gone") }, "holder socket gone"},
		"ps read error":     {func(w *world) { w.psErrAfter = errors.New("ps socket gone") }, "ps socket gone"},
		"ps not json": {func(w *world) {
			w.present = func(int) (string, string, any) { return "not json", "", readyStatus() }
		}, "invalid cotal ps envelope"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(foreman(1), idleSeat("accounts", 10))
			tc.configure(w)
			w.run(t.Context())
			assertCalls(t, w, "cotal stop --name accounts --space main")
			if len(w.logs) != 1 || !strings.Contains(w.logs[0], "liveness unknown after stop") || !strings.Contains(w.logs[0], tc.reason) {
				t.Fatalf("logs = %q, want liveness unknown naming %q", w.logs, tc.reason)
			}
		})
	}
}

func TestStepUnavailableManagerHoldsWait(t *testing.T) {
	w := newWorld(foreman(1), idleSeat("accounts", 10))
	w.present = func(int) (string, string, any) { return psEnvelope(""), "", map[string]any{"state": "idle"} }
	w.run(t.Context())
	assertCalls(t, w, "cotal stop --name accounts --space main")
	if len(w.logs) != 1 || !strings.Contains(w.logs[0], "manager status unavailable after stop") {
		t.Fatalf("logs = %q", w.logs)
	}
}

// TestWaitDeadlineIsWallClock: each probe costs 20 s, so probes start at 0, 21 s and 42 s, and the
// third ends at 62 s, past the 60 s bound.
func TestWaitDeadlineIsWallClock(t *testing.T) {
	w := newWorld(foreman(1), idleSeat("accounts", 10))
	w.present = func(int) (string, string, any) { return psEnvelope(`{"name":"accounts"}`), "", readyStatus() }
	w.probeCost = 20 * time.Second
	deps := w.deps()
	started := w.clock
	err := deps.sequence().waitAbsent(t.Context(), "accounts")
	if err == nil || !strings.Contains(err.Error(), "still present after stop") {
		t.Fatalf("waitAbsent err = %v", err)
	}
	if w.presenceN != 3 || w.clock.Sub(started) != 62*time.Second {
		t.Fatalf("probes = %d elapsed = %v, want 3 and 62s", w.presenceN, w.clock.Sub(started))
	}
}

// TestWaitClockStartsAfterStop: a stop that takes the whole bound must not leave the wait with no
// budget, which would leave the seat stopped.
func TestWaitClockStartsAfterStop(t *testing.T) {
	w := newWorld(foreman(1), idleSeat("accounts", 10))
	polls := 0
	w.present = func(int) (string, string, any) {
		polls++
		if polls < 2 {
			return psEnvelope(`{"name":"accounts"}`), "", readyStatus()
		}
		return psEnvelope(""), "", readyStatus()
	}
	deps := w.deps()
	stop := deps.Stop
	deps.Stop = func(ctx context.Context, name string) error {
		w.clock = w.clock.Add(stopBound)
		return stop(ctx, name)
	}
	Run(t.Context(), deps)
	assertCalls(t, w, "cotal stop --name accounts --space main", "wave-seats restart accounts")
}

func TestStepRestartFailureIsLogged(t *testing.T) {
	w := newWorld(foreman(1), idleSeat("accounts", 10))
	w.restartRC = 1
	w.run(t.Context())
	if len(w.logs) != 1 || !strings.Contains(w.logs[0], "wave-seats restart exited 1") {
		t.Fatalf("logs = %q", w.logs)
	}
}

// TestStepRestartErrorNamesConfiguredSeatCLI pins the failure text to the configured seat CLI, not
// the literal wave-seats. A different name must appear in the log.
func TestStepRestartErrorNamesConfiguredSeatCLI(t *testing.T) {
	w := newWorld(foreman(1), idleSeat("accounts", 10))
	w.restartRC = 1
	deps := w.deps()
	deps.SeatCLI = "seatctl"
	Run(t.Context(), deps)
	if len(w.logs) != 1 || !strings.Contains(w.logs[0], "seatctl restart exited 1") || strings.Contains(w.logs[0], "wave-seats") {
		t.Fatalf("logs = %q, want the configured seatctl name", w.logs)
	}
}

func TestStepRejectsBadThreshold(t *testing.T) {
	w := newWorld(foreman(1), idleSeat("accounts", 10))
	w.threshold = "9.5"
	w.run(t.Context())
	assertCalls(t, w)
	if len(w.logs) != 1 || !strings.Contains(w.logs[0], ThresholdEnv) {
		t.Fatalf("logs = %q", w.logs)
	}
}

func TestStepRosterIdleRelaunchesWithoutHandoff(t *testing.T) {
	w := newWorld(foreman(1), seat{name: "accounts", gib: 10, idle: true})
	w.run(t.Context())
	assertCalls(t, w, "cotal stop --name accounts --space main", "wave-seats restart accounts")
	assertLogs(t, w, outcomeLine("relaunched", "accounts", 10))
}

func TestStepBackgroundRunningIsNotIdle(t *testing.T) {
	w := newWorld(foreman(1), seat{name: "accounts", gib: 10, idle: true, background: true})
	w.run(t.Context())
	assertCalls(t, w)
	assertLogs(t, w, outcomeLine("skipped:working", "accounts", 10))
}

func TestStepNonSeatsRosterFails(t *testing.T) {
	w := newWorld(foreman(1), idleSeat("accounts", 10))
	w.psOverride = func(int) string {
		return `{"schemaVersion":1,"command":"ps","outcome":"ok","result":{"kind":"presence","items":[]}}`
	}
	w.run(t.Context())
	assertCalls(t, w)
	if len(w.logs) != 1 || !strings.Contains(w.logs[0], "outcome=failed") {
		t.Fatalf("logs = %q", w.logs)
	}
}

// TestBoundedGivesUpOnReadThatIgnoresContext: the read never looks at ctx, so only the select in
// bounded can return.
func TestBoundedGivesUpOnReadThatIgnoresContext(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stuck := func(context.Context) (string, error) {
		<-release
		return "", nil
	}
	started := time.Now()
	_, err := bounded(t.Context(), 20*time.Millisecond, stuck)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded err = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("bounded held the run for %v", elapsed)
	}
}
