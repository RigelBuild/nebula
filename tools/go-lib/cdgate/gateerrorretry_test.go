package cdgate

import (
	"context"
	"errors"
	"testing"
)

// TestRunCDGateErrorStillRetriesPulumiTransient: a gate that errors fails open
// to a deploy, and the pulumi up retry still runs. A throttled first attempt is
// retried and the second succeeds, so the run exits 0 after two executions.
func TestRunCDGateErrorStillRetriesPulumiTransient(t *testing.T) {
	s := &scripted{t: t, results: []StepResult{
		{Code: 1, Output: transientOutput},
		{Code: 0, Output: "ok"},
	}}
	r, _ := runFixture(t, Verdict{}, s.exec)
	r.Gate = func(context.Context) (Verdict, error) {
		return Verdict{}, errors.New("API failed")
	}
	if code := RunCD(context.Background(), "aws:up", r); code != 0 {
		t.Fatalf("RunCD = %d, want 0 after the retry succeeds", code)
	}
	if s.calls != 2 {
		t.Fatalf("executions = %d, want 2: the gate error must not stop the transient retry", s.calls)
	}
}
