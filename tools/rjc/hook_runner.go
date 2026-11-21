package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec" //nolint:depguard // Hook commands use argument vectors.
	"path/filepath"
	"strings"

	"github.com/RigelBuild/nebula/tools/go-lib/gatetier"
)

type HookPhase string

const (
	phaseNoConfig    HookPhase = "no-config"
	phaseSetupFailed HookPhase = "setup-failed"
	phaseCompleted   HookPhase = "runner-completed"
	phaseFixup       HookPhase = "fixup"
	phaseToolFailed  HookPhase = "tool-failed"
)

type HookOutcome struct {
	Phase         HookPhase `json:"phase"`
	Exit          int       `json:"exit"`
	From          string    `json:"from"`
	To            string    `json:"to"`
	Config        string    `json:"config"`
	Fixup         string    `json:"fixup"`
	FailedTargets []string  `json:"-"`
	FailedGates   []string  `json:"-"`
}

type HookRunner interface {
	Run(ctx context.Context, base, head string, skip []SkippedTarget, log io.Writer) (HookOutcome, RunRef, error)
}

type localHookRunner struct{ repo *repository }

func (r localHookRunner) Run(ctx context.Context, base, head string, skip []SkippedTarget, log io.Writer) (_ HookOutcome, _ RunRef, retErr error) {
	var outcome HookOutcome
	if !objectID.MatchString(base) || !objectID.MatchString(head) {
		return outcome, RunRef{}, toolFailure("hooks", errors.New("invalid hook range"))
	}
	dir, err := os.MkdirTemp("", "rjc-hooks-")
	if err != nil {
		return outcome, RunRef{}, toolFailure("hooks", err)
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(dir)) }()
	result, moonReport, gateReport := filepath.Join(dir, "result.json"), filepath.Join(dir, "moon.json"), filepath.Join(dir, "gates.json")
	env := []string{"MOON_BASE=" + base, "MOON_HEAD=" + head, "RJC_MOON_REPORT=" + moonReport, "RJC_MOON_TARGETS=", "HK_FILE=hk.pkl"}
	if r.repo.profile.HookReports {
		env = append(env, "RJC_GATE_REPORT="+gateReport)
	}
	var remaining []string
	if len(skip) != 0 {
		remaining, err = r.targets(ctx, base, head, skip)
		if err != nil {
			return outcome, RunRef{}, err
		}
		env[3] = "RJC_MOON_TARGETS=" + strings.Join(remaining, ",")
	}
	releaseSlot, err := acquireLocalGateSlot(ctx, r.repo.env, log)
	if err != nil {
		return outcome, RunRef{}, toolFailure("hooks", err)
	}
	defer func() { retErr = errors.Join(retErr, releaseSlot()) }()
	cmd := r.repo.execCommand(ctx, "jj-hp", "run", "--stage", "pre-push", "--no-retry-after-fixup", "--from", base, "--to", head, "--result-json", result)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = r.repo.root, r.repo.childEnv(env), log, log
	runErr := cmd.Run()
	if runErr != nil {
		var exit *exec.ExitError
		if !errors.As(runErr, &exit) || exit.ExitCode() < 0 {
			return outcome, RunRef{}, toolFailure("jj-hp", runErr)
		}
	}
	data, err := os.ReadFile(result) //nolint:gosec // The result path is in a private temporary directory.
	if err != nil {
		return outcome, RunRef{}, toolFailure("jj-hp", err)
	}
	if err := json.Unmarshal(data, &outcome); err != nil {
		return outcome, RunRef{}, toolFailure("jj-hp", err)
	}
	if outcome.Phase == phaseToolFailed {
		reason := "reported tool failure (tool-failed phase)"
		if filepath.Base(outcome.Config) == hkConfigName {
			if _, err := os.Stat(filepath.Join(r.repo.root, ".envrc")); err == nil && r.repo.execCommand(ctx, "hk").Err != nil {
				reason += "; run inside the repo dev shell (direnv exec .)"
			}
		}
		return outcome, RunRef{}, toolFailure("jj-hp", errors.New(reason))
	}
	if outcome.From != base || outcome.To != head || outcome.Phase == phaseSetupFailed {
		return outcome, RunRef{}, toolFailure("jj-hp", errors.New("invalid hook result or setup failed"))
	}
	if outcome.Phase != phaseCompleted && outcome.Phase != phaseNoConfig && outcome.Phase != phaseFixup {
		return outcome, RunRef{}, toolFailure("jj-hp", fmt.Errorf("unknown hook phase %q", outcome.Phase))
	}
	if outcome.Phase == phaseCompleted && (r.repo.profile.HookReports || len(skip) > 0) {
		outcome.FailedTargets, err = readMoonReport(moonReport, remaining, skip)
		if err != nil {
			return outcome, RunRef{}, toolFailure("hooks", err)
		}
	}
	if outcome.Phase == phaseCompleted && r.repo.profile.HookReports {
		outcome.FailedGates, err = readGateReport(gateReport)
		if err != nil {
			return outcome, RunRef{}, toolFailure("hooks", err)
		}
	}
	return outcome, RunRef{}, nil
}
func (r localHookRunner) targets(ctx context.Context, base, head string, skip []SkippedTarget) ([]string, error) {
	planner := gatetier.Planner{Runner: func(ctx context.Context, env []string, args ...string) ([]byte, error) {
		return r.repo.command(ctx, env, nil, "moon", args...)
	}}
	plan, err := planner.Plan(ctx, gatetier.Fast, base, head)
	if err != nil {
		return nil, err
	}
	targets := make(map[string]struct{}, len(plan))
	for _, task := range plan {
		targets[task.Target] = struct{}{}
	}
	omitted := make(map[string]bool, len(skip))
	for _, target := range skip {
		if _, ok := targets[target.Target]; !ok {
			return nil, refusal("hooks", "skip-target-not-affected", target.Target+" is not affected", "select an affected target")
		}
		omitted[target.Target] = true
	}
	remaining := make([]string, 0, len(plan))
	for _, task := range plan {
		target := task.Target
		if omitted[target] {
			continue
		}
		for _, dependency := range task.Deps {
			if omitted[dependency] {
				return nil, refusal("hooks", "skip-target-has-dependents", target+" depends on "+dependency, "include the dependency")
			}
		}
		remaining = append(remaining, target)
	}
	if len(remaining) == 0 {
		return nil, refusal("hooks", "skip-target-empties-run", "skips leave no targets", "run at least one target")
	}
	return remaining, nil
}

func readMoonReport(path string, expected []string, skip []SkippedTarget) ([]string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // The report path is in a private temporary directory.
	if err != nil {
		return nil, err
	}
	var report struct {
		Actions []struct {
			Status string `json:"status"`
			Node   struct {
				Action string `json:"action"`
				Params struct {
					Target string `json:"target"`
				} `json:"params"`
			} `json:"node"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(data, &report); err != nil || report.Actions == nil {
		return nil, errors.New("invalid moon report")
	}
	seen := map[string]bool{}
	failed := []string{}
	for _, action := range report.Actions {
		if action.Node.Action != "run-task" {
			continue
		}
		target := action.Node.Params.Target
		seen[target] = true
		// moon's ActionStatus: only these mean the task succeeded; anything else counts as failed.
		if action.Status != "passed" && action.Status != "cached" && action.Status != "cached-from-remote" {
			failed = append(failed, target)
		}
	}
	for _, target := range expected {
		if !seen[target] {
			return nil, fmt.Errorf("moon report missing %s", target)
		}
	}
	for _, target := range skip {
		if seen[target.Target] {
			return nil, fmt.Errorf("moon report ran skipped target %s", target.Target)
		}
	}
	return failed, nil
}

func readGateReport(path string) ([]string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // The report path is in a private temporary directory.
	if err != nil {
		return nil, err
	}
	var gates []struct {
		Name string `json:"name"`
		Exit int    `json:"exit"`
	}
	if err := json.Unmarshal(data, &gates); err != nil || gates == nil {
		return nil, errors.New("invalid gate report")
	}
	failed := []string{}
	for _, gate := range gates {
		if gate.Exit != 0 {
			failed = append(failed, gate.Name)
		}
	}
	return failed, nil
}
