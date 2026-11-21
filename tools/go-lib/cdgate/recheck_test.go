package cdgate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RigelBuild/nebula/tools/go-lib/woodpecker"
)

// TestRunCDLostLeaseDuringRecheckBlocksDeploy: the gate runs again under the held
// Lease. If the Lease is taken while that re-check is still running, the deploy
// must not run. The first gate deploys; the re-check blocks until the Lease is
// lost, then the step returns 1 without executing the command.
func TestRunCDLostLeaseDuringRecheckBlocksDeploy(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1"}}
	clk := &fakeClock{}
	var waits []time.Duration
	base := clk.sleeper(&waits)
	gates := 0
	executed := false
	// The second gate call (the re-check, under the Lease) blocks until the test
	// takes the Lease. The renewal then sees another holder and reports the loss.
	recheckStarted := make(chan struct{})
	var steal sync.Once
	var logBuf bytes.Buffer
	r := Run{
		Gate: func(ctx context.Context) (Verdict, error) {
			gates++
			if gates == 1 {
				return Verdict{Deploy: true}, nil
			}
			close(recheckStarted)
			// Blocks until the loss cancels the re-check, as a slow live gate would.
			<-ctx.Done()
			return Verdict{}, ctx.Err()
		},
		Exec: func(context.Context, bool) (StepResult, error) {
			executed = true
			return StepResult{Code: 0}, nil
		},
		Clock: Clock{Wall: clk.wall, Monotonic: clk.monotonic},
		Sleep: func(ctx context.Context, d time.Duration) error {
			// The first renewal wait waits for the re-check to start, then takes the
			// Lease once. Waiting, not polling, keeps the steal ordered before the gate returns.
			steal.Do(func() {
				<-recheckStarted
				api.record = &LeaseRecord{ResourceVersion: "2", HolderIdentity: "99/other/pod-9"}
				// The gate stays blocked: the loss cancels it, and a cancelled gate never deploys.
			})
			return base(ctx, d)
		},
		Lease: lease(api),
		Log:   &logBuf,
	}
	code := RunCD(context.Background(), "nix:deploy", r)
	if executed {
		t.Fatal("the deploy ran after the Lease was lost during the re-check")
	}
	if code == 0 {
		t.Fatalf("RunCD = 0 after a lost Lease during the re-check; want a failing exit")
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "cd-lease: lost before deploy") || strings.Contains(logs, "stopped by signal") {
		t.Fatalf("log = %q; want the lease-loss message, not the signal message", logs)
	}
}

// TestRunCDRecheckInvalidAPIContextBlocksAndReleasesLease: the gate deploys on
// the first read, then the re-check under the held Lease reports an invalid
// API context. The step must exit 1, never run the deploy, and release the
// Lease it took. The release is the last Lease write, with the holder cleared.
func TestRunCDRecheckInvalidAPIContextBlocksAndReleasesLease(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1"}}
	s := &scripted{t: t, results: []StepResult{{Code: 0}}}
	gates := 0
	var logBuf bytes.Buffer
	r := Run{
		Gate: func(context.Context) (Verdict, error) {
			gates++
			if gates == 1 {
				return Verdict{Deploy: true}, nil
			}
			return Verdict{}, fmt.Errorf("resolving CD API context: %w", woodpecker.ErrInvalidAPIContext)
		},
		Exec:         s.exec,
		PulumiStacks: map[string]bool{"aws": true},
		Clock:        Clock{Wall: (&fakeClock{}).wall, Monotonic: (&fakeClock{}).monotonic},
		Sleep:        (&fakeClock{}).sleeper(new([]time.Duration)),
		Log:          &logBuf,
		Lease:        lease(api),
	}
	if code := RunCD(context.Background(), "nix:deploy", r); code != 1 {
		t.Fatalf("RunCD = %d, want 1: an invalid API context under the Lease must block", code)
	}
	if s.calls != 0 {
		t.Fatalf("deploy ran %d times after the re-check blocked", s.calls)
	}
	if gates != 2 {
		t.Fatalf("gate calls = %d, want 2: the first read and the re-check under the Lease", gates)
	}
	if len(api.writes) == 0 {
		t.Fatal("no Lease writes: the step never took the Lease it must release")
	}
	last := api.writes[len(api.writes)-1]
	if last.HolderIdentity != "" {
		t.Fatalf("final Lease write = %+v, want the holder cleared on release", last)
	}
	if !strings.Contains(logBuf.String(), "cd-gate: blocked") {
		t.Fatalf("log = %q, want the cd-gate: blocked line", logBuf.String())
	}
}

func TestRecheckUnderLeaseGateErrorFailsOpen(t *testing.T) {
	r, _ := runFixture(t, Verdict{}, nil)
	r.Gate = func(context.Context) (Verdict, error) { return Verdict{}, errors.New("recheck down") }
	lost := make(chan struct{})
	code, done := recheckUnderLease(t.Context(), r, lost)
	if done || code != 0 {
		t.Fatalf("recheckUnderLease = (%d, %v), want (0, false): a recheck gate error fails open", code, done)
	}
}
