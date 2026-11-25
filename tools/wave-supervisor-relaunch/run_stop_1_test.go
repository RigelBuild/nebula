package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/cotalreach"
)

// writeTestConfig writes testConfig as a CUE file and returns its path. The file goes through
// loadConfig and the closed #Config schema, the same path a host config takes.
func writeTestConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relaunch.cue")
	cfg := testConfig()
	body := fmt.Sprintf(`rootSeat: %q
cotalSpace: %q
endpointKind: %q
zellijSession: %q
zellijUnit: %q
cotalManagerUnit: %q
relaunchUnit: %q
waveSlice: %q
seatsPath: %q
stateDir: %q
homeDir: %q
sessionsDir: %q
hosts: [%q]
managerLogPath: %q
journalUnits: [%q, %q, %q]
relaunchIssue: %q
knownCause: %q
causeOwner: %q
seatCLI: %q
spawnCommand: %q
seatHolderCLI: %q
`, cfg.RootSeat, cfg.CotalSpace, cfg.EndpointKind, cfg.ZellijSession, cfg.ZellijUnit, cfg.CotalManagerUnit, cfg.RelaunchUnit, cfg.WaveSlice, cfg.SeatsPath, cfg.StateDir, cfg.HomeDir, cfg.SessionsDir, cfg.Hosts[0], cfg.ManagerLogPath, cfg.JournalUnits[0], cfg.JournalUnits[1], cfg.JournalUnits[2], cfg.RelaunchIssue, cfg.KnownCause, cfg.CauseOwner, cfg.SeatCLI, cfg.SpawnCommand, cfg.SeatHolderCLI)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStopPostEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name              string
		serviceResult     string
		serviceExitStatus string
		wantFailure       bool
	}{
		{name: "timeout reports failure", serviceResult: "timeout", wantFailure: true},
		{name: "terminated signal stays quiet", serviceResult: "signal", serviceExitStatus: "TERM"},
		{name: "success stays quiet", serviceResult: "success"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{
				"HOME":                t.TempDir(),
				"SERVICE_RESULT":      tc.serviceResult,
				"SERVICE_EXIT_STATUS": tc.serviceExitStatus,
			}
			var stdout, stderr strings.Builder
			status := run(t.Context(), []string{"--config", writeTestConfig(t), "stop-post"}, func(key string) string { return env[key] }, &stdout, &stderr)
			if status != 0 {
				t.Fatalf("run(stop-post) = %d, want 0; stderr=%q", status, stderr.String())
			}
			if got := strings.Contains(stdout.String(), "outcome=failed"); got != tc.wantFailure {
				t.Fatalf("stop-post output = %q, contains failed marker = %t, want %t", stdout.String(), got, tc.wantFailure)
			}
		})
	}
}

func TestStopManagedSeatsBestEffortFailure(t *testing.T) {
	t.Run("child stop failure is returned for caller to log while slice remains available", func(t *testing.T) {
		runner := func(context.Context, []string, string) (cotalreach.CommandResult, error) {
			return cotalreach.CommandResult{ExitCode: 1, Stderr: "failed"}, nil
		}
		live := func(context.Context, string) (int, error) { return 0, nil }
		if err := stopManagedSeats(context.Background(), []string{"accounts"}, "main", runner, live); err == nil {
			t.Fatal("expected stop failure")
		}
	})
}

func TestStopManagedSeatsBoundedFakeProcesses(t *testing.T) {
	t.Run("stops at most eight children concurrently", testStopManagedSeatsBoundedFakeProcessesCase1)
	t.Run("caller cancellation cancels every active fake child", testStopManagedSeatsBoundedFakeProcessesCase2)
	t.Run("overall deadline cancels active fake children and prevents queued starts", testStopManagedSeatsBoundedFakeProcessesCase3)
}
func testStopManagedSeatsBoundedFakeProcessesCase1(t *testing.T) {
	active, maximum, started := 0, 0, 0
	var mu sync.Mutex
	allStarted := make(chan struct{})
	release := make(chan struct{})
	var runner processRunner = func(_ context.Context, _ []string, _ string) (cotalreach.CommandResult, error) {
		mu.Lock()
		active++
		started++
		if active > maximum {
			maximum = active
		}
		if started == 8 {
			close(allStarted)
		}
		mu.Unlock()
		<-release
		mu.Lock()
		active--
		mu.Unlock()
		return cotalreach.CommandResult{}, nil
	}
	live := func(context.Context, string) (int, error) { return 0, nil }
	done := make(chan error, 1)
	go func() {
		done <- stopManagedSeats(context.Background(), []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}, "main", runner, live)
	}()
	<-allStarted
	mu.Lock()
	got := maximum
	mu.Unlock()
	if got != 8 {
		t.Fatalf("max concurrency=%d", got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func testStopManagedSeatsBoundedFakeProcessesCase2(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{}, 3)
	var runner processRunner = func(ctx context.Context, _ []string, _ string) (cotalreach.CommandResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return cotalreach.CommandResult{}, ctx.Err()
	}
	live := func(context.Context, string) (int, error) { return 0, nil }
	done := make(chan error, 1)
	go func() { done <- stopManagedSeats(ctx, []string{"a", "b", "c"}, "main", runner, live) }()
	for range 3 {
		<-started
	}
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stopManagedSeats() error = %v, want context.Canceled", err)
	}
}
func testStopManagedSeatsBoundedFakeProcessesCase3(t *testing.T) {
	testCtx := t.Context()
	deadlineCtx, cancelDeadline := context.WithCancel(testCtx)
	defer cancelDeadline()
	started := make(chan string, 10)
	var active atomic.Int32
	var maxActive atomic.Int32
	var runner processRunner = func(ctx context.Context, args []string, _ string) (cotalreach.CommandResult, error) {
		current := active.Add(1)
		for {
			prior := maxActive.Load()
			if current <= prior || maxActive.CompareAndSwap(prior, current) {
				break
			}
		}
		started <- args[3]
		<-ctx.Done()
		active.Add(-1)
		return cotalreach.CommandResult{}, ctx.Err()
	}
	live := func(context.Context, string) (int, error) { return 0, nil }
	done := make(chan error, 1)
	go func() {
		done <- stopManagedSeatsWithDeadline(testCtx, []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}, "main", runner, live, 50*time.Millisecond, func(context.Context, time.Duration) (context.Context, context.CancelFunc) {
			return deadlineCtx, cancelDeadline
		})
	}()
	first := make(map[string]struct{}, 8)
	for range 8 {
		first[<-started] = struct{}{}
	}
	cancelDeadline()
	err := <-done
	if !errors.Is(err, context.Canceled) || active.Load() != 0 || len(first) != 8 || maxActive.Load() != 8 {
		t.Fatalf("err=%v active=%d started=%d max=%d", err, active.Load(), len(first), maxActive.Load())
	}
	select {
	case name := <-started:
		t.Fatalf("queued child %q started after the deadline", name)
	default:
	}
}
