package cdgate

import (
	"context"
	"errors"
	"testing"
)

// TestRunCDNonZeroExitStillReleases: a deploy that exits non-zero still releases
// the Lease, so the next holder is not stalled for the full duration.
func TestRunCDNonZeroExitStillReleases(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1"}}
	s := &scripted{t: t, results: []StepResult{{Code: 3}}}
	r, _ := runFixture(t, Verdict{Deploy: true}, s.exec)
	r.Lease = lease(api)
	if code := RunCD(context.Background(), "nix:deploy", r); code != 3 {
		t.Fatalf("RunCD = %d, want the deploy's exit 3", code)
	}
	if last := api.writes[len(api.writes)-1]; last.HolderIdentity != "" {
		t.Fatalf("final Lease write = %+v, want the release after a non-zero exit", last)
	}
}

// TestRunCDThrowingExecuteStillReleases: an executor that returns an error, not
// an exit code, still releases the Lease, and the step fails.
func TestRunCDThrowingExecuteStillReleases(t *testing.T) {
	api := &memLease{record: &LeaseRecord{ResourceVersion: "1"}}
	boom := errors.New("exec: spawn failed")
	exec := func(context.Context, bool) (StepResult, error) { return StepResult{}, boom }
	r, _ := runFixture(t, Verdict{Deploy: true}, exec)
	r.Lease = lease(api)
	if code := RunCD(context.Background(), "nix:deploy", r); code != 1 {
		t.Fatalf("RunCD = %d, want 1 when the executor throws", code)
	}
	if last := api.writes[len(api.writes)-1]; last.HolderIdentity != "" {
		t.Fatalf("final Lease write = %+v, want the release after a throw", last)
	}
}
