// SPDX-License-Identifier: MIT OR Apache-2.0

package cdgate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/RigelBuild/nebula/tools/go-lib/transientretry"
)

// StepResult is one run of the deploy command.
type StepResult struct {
	Code     int
	Output   string
	Overflow bool
}

// Executor runs the deploy command once. capture asks for the output, so the
// retry classifier can read it. The callee returns early on ctx cancel.
type Executor func(ctx context.Context, capture bool) (StepResult, error)

// ErrDeployStopped reports a stop that arrived before a retry attempt started.
var ErrDeployStopped = errors.New("deploy stopped")

// stepFailedError carries a non-zero exit from one pulumi up attempt. The field
// names match what the retry classifier reads. An overflowed capture counts as
// a started resource step, because its output cannot be checked.
type stepFailedError struct {
	exitCode             int
	output               string
	resourceStepsStarted bool
}

func (e *stepFailedError) Error() string {
	lines := strings.Split(strings.TrimSpace(e.output), "\n")
	if last := lines[len(lines)-1]; last != "" {
		return last
	}
	return fmt.Sprintf("step exited %d", e.exitCode)
}

// Deploy runs the command. Only registered Pulumi up targets retry, and only
// when output proves no resource step ran. A stop before a retry attempt keeps
// the attempt from starting. The retry returns the step's exit code.
func Deploy(ctx context.Context, target string, pulumiStacks map[string]bool, exec Executor, sleep Sleeper) (int, error) {
	if !isPulumiUp(target, pulumiStacks) {
		res, err := exec(ctx, false)
		return res.Code, err
	}
	bounds := transientretry.DefaultBounds
	op := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return ErrDeployStopped
		}
		res, err := exec(ctx, true)
		if err != nil {
			return err
		}
		if res.Code != 0 {
			return &stepFailedError{exitCode: res.Code, output: res.Output, resourceStepsStarted: res.Overflow}
		}
		return nil
	}
	classify := func(err error) transientretry.Class {
		if ctx.Err() != nil {
			return ""
		}
		return pulumiClass(err)
	}
	err := transientretry.Do(ctx, op, classify, bounds, transientretry.Sleeper(sleep))
	if err == nil {
		return 0, nil
	}
	if failed, ok := errors.AsType[*stepFailedError](err); ok {
		return failed.exitCode, nil
	}
	if errors.Is(err, ErrDeployStopped) || errors.Is(err, context.Canceled) {
		return 1, nil
	}
	return 0, err
}

// pulumiClass adapts a step failure to the Pulumi retry classifier. The
// classifier reads a resourceStepsStarted flag from the error, so the
// stepFailedError value is mapped onto transientretry.DiagnosticError.
func pulumiClass(err error) transientretry.Class {
	if failed, ok := errors.AsType[*stepFailedError](err); ok {
		return transientretry.ClassifyPulumiTransient(&transientretry.DiagnosticError{
			Message:              failed.output,
			ResourceStepsStarted: failed.resourceStepsStarted,
		})
	}
	return ""
}
