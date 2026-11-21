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

	"github.com/RigelBuild/nebula/tools/go-lib/kubeserviceaccount"
	"github.com/RigelBuild/nebula/tools/go-lib/woodpecker"
)

// runFixture builds a Run with a fake clock, a no-op sleeper and a gate that
// returns verdict. Each test overrides the field it exercises.
func runFixture(t *testing.T, verdict Verdict, exec Executor) (Run, *bytes.Buffer) {
	t.Helper()
	clk := &fakeClock{}
	var waits []time.Duration
	log := &bytes.Buffer{}
	return Run{
		Gate:         func(context.Context) (Verdict, error) { return verdict, nil },
		Exec:         exec,
		Clock:        Clock{Wall: clk.wall, Monotonic: clk.monotonic},
		Sleep:        clk.sleeper(&waits),
		PulumiStacks: map[string]bool{"aws": true},
		Log:          log,
	}, log
}

func TestLockedWriterConcurrentWrites(t *testing.T) {
	var log bytes.Buffer
	writer := &lockedWriter{w: &log}
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			logf(writer, "concurrent log entry")
		})
	}
	wg.Wait()
	if got := strings.Count(log.String(), "concurrent log entry\n"); got != 2 {
		t.Fatalf("log has %d complete entries, want 2: %q", got, log.String())
	}
}

// TestRunCDSkipVerdictReturnsSkipExitCodeWithoutLease pins the wire value 79:
// Woodpecker's skip_exit_code maps it to a skipped step (fork woodpecker#137).
func TestRunCDSkipVerdictReturnsSkipExitCodeWithoutLease(t *testing.T) {
	api := &memLease{record: heldBy("42/step/pod-1")}
	s := &scripted{t: t}
	r, log := runFixture(t, Verdict{Deploy: false, Reason: ReasonSuperseded, By: 99}, s.exec)
	var skipped Verdict
	r.Skip = func(v Verdict) { skipped = v }
	r.Lease = lease(api)
	if code := RunCD(context.Background(), "aws:up", r); code != 79 {
		t.Fatalf("RunCD = %d, want 79 (skip_exit_code) for a skip", code)
	}
	if skipped.Reason != ReasonSuperseded {
		t.Fatalf("skip reason = %q", skipped.Reason)
	}
	if s.calls != 0 || len(api.writes) != 0 {
		t.Fatalf("a skipped run touched the deploy (%d) or the Lease (%d writes)", s.calls, len(api.writes))
	}
	if !strings.Contains(log.String(), "skipped superseded 99") {
		t.Fatalf("log = %q, want the superseded skip line", log.String())
	}
}

func TestLogSkipNamesSupersedingPipeline(t *testing.T) {
	var log bytes.Buffer
	logSkip(&log, Verdict{Deploy: false, Reason: ReasonSuperseded, By: 7})
	if got := log.String(); got != "cd-gate: skipped superseded 7\n" {
		t.Fatalf("log = %q, want the superseding pipeline number", got)
	}
}

func TestRunCDGateErrorFailsOpenAndDeploys(t *testing.T) {
	s := &scripted{t: t, results: []StepResult{{Code: 0}}}
	r, log := runFixture(t, Verdict{}, s.exec)
	r.Gate = func(context.Context) (Verdict, error) { return Verdict{}, errors.New("gate down") }
	if code := RunCD(context.Background(), "nix:deploy", r); code != 0 {
		t.Fatalf("RunCD = %d, want 0 after a fail-open deploy", code)
	}
	if s.calls != 1 {
		t.Fatalf("calls = %d, want the deploy to run after a gate error", s.calls)
	}
	if !strings.Contains(log.String(), "gate error: gate down") {
		t.Fatalf("log = %q, want the gate error line", log.String())
	}
}

func TestRunCDMissingCredentialBlocksDeploy(t *testing.T) {
	s := &scripted{t: t}
	r, log := runFixture(t, Verdict{}, s.exec)
	r.Gate = func(context.Context) (Verdict, error) {
		return Verdict{}, fmt.Errorf("resolving CD API context: %w", woodpecker.ErrMissingCredential)
	}
	if code := RunCD(context.Background(), "nix:deploy", r); code != 1 {
		t.Fatalf("RunCD = %d, want 1: a missing CD credential must block", code)
	}
	if s.calls != 0 {
		t.Fatalf("deploy ran %d times without a credential", s.calls)
	}
	if !strings.Contains(log.String(), "cd-gate: blocked") {
		t.Fatalf("log = %q, want the cd-gate: blocked line", log.String())
	}
}

func TestRunCDInvalidAPIContextBlocksDeploy(t *testing.T) {
	s := &scripted{t: t}
	r, _ := runFixture(t, Verdict{}, s.exec)
	r.Gate = func(context.Context) (Verdict, error) {
		return Verdict{}, fmt.Errorf("resolving CD API context: %w", woodpecker.ErrInvalidAPIContext)
	}
	if code := RunCD(context.Background(), "nix:deploy", r); code != 1 {
		t.Fatalf("RunCD = %d, want 1: a malformed API context must block", code)
	}
	if s.calls != 0 {
		t.Fatalf("deploy ran %d times with an invalid API context", s.calls)
	}
}

func TestRunCDPrepareLeaseFailureReturnsOneWithoutDeploy(t *testing.T) {
	s := &scripted{t: t}
	r, _ := runFixture(t, Verdict{Deploy: true}, s.exec)
	r.PrepareLease = func() (*LeaseConfig, error) { return nil, errors.New("no token") }
	if code := RunCD(context.Background(), "aws:up", r); code != 1 {
		t.Fatalf("RunCD = %d, want 1 when lease setup fails", code)
	}
	if s.calls != 0 {
		t.Fatalf("deploy ran %d times after a lease setup failure", s.calls)
	}
}

func TestRunCDNilPreparedLeaseDeploysWithoutLease(t *testing.T) {
	s := &scripted{t: t, results: []StepResult{{Code: 0}}}
	r, log := runFixture(t, Verdict{Deploy: true}, s.exec)
	r.PrepareLease = func() (*LeaseConfig, error) { return nil, kubeserviceaccount.ErrNotKubernetes }
	if code := RunCD(context.Background(), "nix:deploy", r); code != 0 {
		t.Fatalf("RunCD = %d, want 0 with no lease", code)
	}
	if !strings.Contains(log.String(), "KUBERNETES_SERVICE_HOST unset") {
		t.Fatalf("log = %q, want the no-lease line", log.String())
	}
}

func TestRunCDFailedAcquireReturnsOneWithoutRelease(t *testing.T) {
	// A Lease read that fails outright is not a takeover, so the acquire fails
	// and returns 1. This step never held the Lease, so it must not write to it.
	api := &memLease{getErr: errors.New("apiserver unavailable")}
	s := &scripted{t: t}
	r, _ := runFixture(t, Verdict{Deploy: true}, s.exec)
	r.Lease = lease(api)
	if code := RunCD(context.Background(), "aws:up", r); code != 1 {
		t.Fatalf("RunCD = %d, want 1 when acquire fails", code)
	}
	if s.calls != 0 {
		t.Fatalf("deploy ran %d times without the Lease", s.calls)
	}
	if len(api.writes) != 0 {
		t.Fatalf("a step that never held the Lease wrote to it: %+v", api.writes)
	}
}

func TestRunCDSuccessfulDeployReleasesLease(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1"}}
	s := &scripted{t: t, results: []StepResult{{Code: 0}}}
	r, _ := runFixture(t, Verdict{Deploy: true}, s.exec)
	r.Lease = lease(api)
	if code := RunCD(context.Background(), "nix:deploy", r); code != 0 {
		t.Fatalf("RunCD = %d, want 0", code)
	}
	last := api.writes[len(api.writes)-1]
	if last.HolderIdentity != "" {
		t.Fatalf("final Lease write = %+v, want the holder cleared on release", last)
	}
}

// failAfterAcquire lets the first write (the acquire) through and fails every
// later write, so the release is the write that fails.
type failAfterAcquire struct {
	*memLease
	updates int
}

func (f *failAfterAcquire) Update(ctx context.Context, w LeaseWrite) (*LeaseRecord, error) {
	f.updates++
	if f.updates > 1 {
		f.memLease.writes = append(f.memLease.writes, w)
		return nil, conflict()
	}
	return f.memLease.Update(ctx, w)
}

func TestRunCDFailedReleaseTurnsGreenIntoFailure(t *testing.T) {
	// The acquire takes a holderless record. The deploy succeeds, but the
	// release write fails, so the Lease stays held. A held Lease would stall the
	// next deploy for the full duration, so the step must fail.
	inner := &memLease{record: &LeaseRecord{ResourceVersion: "1"}}
	api := &failAfterAcquire{memLease: inner}
	s := &scripted{t: t, results: []StepResult{{Code: 0}}}
	r, _ := runFixture(t, Verdict{Deploy: true}, s.exec)
	r.Lease = lease(api)
	if code := RunCD(context.Background(), "nix:deploy", r); code != 1 {
		t.Fatalf("RunCD = %d, want 1 when release fails on a green deploy", code)
	}
}

// TestRunCDFailedReleaseUnderSkipFailsStep: the re-check under the Lease says
// skip (79), then the release write fails. A skip exit must not hide a Lease
// left held, so the step fails with 1.
func TestRunCDFailedReleaseUnderSkipFailsStep(t *testing.T) {
	inner := &memLease{record: &LeaseRecord{ResourceVersion: "1"}}
	api := &failAfterAcquire{memLease: inner}
	gates := 0
	executed := false
	r, log := runFixture(t, Verdict{Deploy: true}, nil)
	r.Gate = func(context.Context) (Verdict, error) {
		gates++
		if gates == 1 {
			return Verdict{Deploy: true}, nil
		}
		return Verdict{Deploy: false, Reason: ReasonRestartNotHead}, nil
	}
	r.Exec = func(context.Context, bool) (StepResult, error) {
		executed = true
		return StepResult{Code: 0}, nil
	}
	r.Lease = lease(api)
	if code := RunCD(context.Background(), "nix:deploy", r); code != 1 {
		t.Fatalf("RunCD = %d, want 1 when the release fails under a skip", code)
	}
	if executed {
		t.Fatal("the command ran after the re-check said skip")
	}
	if !strings.Contains(log.String(), "cd-lease: release failed") {
		t.Fatalf("log = %q, want the release failure line", log.String())
	}
}

func TestRunCDFailedDeployStaysFailed(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1"}}
	s := &scripted{t: t, results: []StepResult{{Code: 5}}}
	r, _ := runFixture(t, Verdict{Deploy: true}, s.exec)
	r.Lease = lease(api)
	if code := RunCD(context.Background(), "nix:deploy", r); code != 5 {
		t.Fatalf("RunCD = %d, want the deploy's exit 5 preserved", code)
	}
}

// TestRunCDDeployExitingSkipCodeFailsStep: a deploy that itself exits 79 must
// not read as a gate skip. The step fails with 1, so only the gate can produce
// the skip code.
func TestRunCDDeployExitingSkipCodeFailsStep(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1"}}
	s := &scripted{t: t, results: []StepResult{{Code: CDSkipExitCode}}}
	r, _ := runFixture(t, Verdict{Deploy: true}, s.exec)
	r.Lease = lease(api)
	if code := RunCD(context.Background(), "nix:deploy", r); code != 1 {
		t.Fatalf("RunCD = %d, want 1 when the deploy itself exits %d", code, CDSkipExitCode)
	}
}

// TestRunCDUnleasedDeployExitingSkipCodeFailsStep: the same rule without a
// Lease. Without this case, deleting deployExit from the no-lease return in
// RunCD leaves every test green.
func TestRunCDUnleasedDeployExitingSkipCodeFailsStep(t *testing.T) {
	s := &scripted{t: t, results: []StepResult{{Code: CDSkipExitCode}}}
	r, _ := runFixture(t, Verdict{Deploy: true}, s.exec)
	if code := RunCD(context.Background(), "nix:deploy", r); code != 1 {
		t.Fatalf("RunCD = %d, want 1 when the unleased deploy itself exits %d", code, CDSkipExitCode)
	}
}
