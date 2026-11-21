package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec" //nolint:depguard // Inspect exit status from the explicit hook command.
	"path/filepath"
	"strings"
)

func leftOut(rec HookRecord) ([]string, []string, bool, error) {
	if rec.Result == hookMainRed {
		if rec.Skip != "" {
			rec.Result = hookSkipped
		} else {
			rec.Result = hookPartial
		}
	}
	switch rec.Result {
	case hookPartial:
		targets := make([]string, 0, len(rec.SkippedTargets))
		for _, skip := range rec.SkippedTargets {
			targets = append(targets, skip.Target)
		}
		if len(targets) == 0 {
			return nil, nil, false, errors.New("partial record has no skipped targets")
		}
		return targets, nil, false, nil
	case hookSkipped:
		if !strings.HasPrefix(rec.Skip, "main-red:") {
			return nil, nil, false, refusal("ready", "hooks-skipped", "only main-red skips take the main-red recheck", "run rjc ready without --main-red")
		}
		if rec.CoveredBy != "" {
			// The covering head's failed targets do not describe this head's own changes, so recheck the whole hook.
			return nil, nil, true, nil
		}
		return rec.FailedTargets, rec.FailedGates, len(rec.FailedTargets) == 0 && len(rec.FailedGates) == 0, nil
	default:
		return nil, nil, false, refusal("ready", "hooks-not-passed", "record is not eligible for red-main recheck", "run the hooks")
	}
}

func (r *repository) recheckMainRed(ctx context.Context, p Profile, rec HookRecord, mainTip string) (passed []string, resultErr error) {
	targets, gates, whole, err := leftOut(rec)
	if err != nil {
		return nil, err
	}
	if len(gates) > 0 && !p.HookReports {
		return nil, refusal("ready", "hook-reports-off", "named gate rechecks require hook reports", "run the hooks without skipping")
	}
	if rec.CoveredBy != "" {
		return r.recheckInherited(ctx, rec)
	}
	dir, err := os.MkdirTemp("", "rjc-main-red-")
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(dir)) }()
	path := filepath.Join(dir, "workspace")
	name := filepath.Base(dir)
	if _, err = r.jj(ctx, "workspace", "add", "--name", name, "--revision", mainTip, path); err != nil {
		return nil, err
	}
	defer func() {
		_, cleanupErr := r.jj(context.WithoutCancel(ctx), "workspace", "forget", name)
		resultErr = errors.Join(resultErr, cleanupErr)
	}()
	workspace := *r
	workspace.root = path
	if whole {
		_, runErr := workspace.command(ctx, []string{"MOON_BASE=" + rec.Base, "MOON_HEAD=" + mainTip}, nil, "hk", "run", "pre-push", "--from-ref", rec.Base, "--to-ref", mainTip)
		if err := recheckExit(runErr); err != nil {
			return nil, err
		}
		if runErr == nil {
			passed = append(passed, "whole hook")
		}
		return passed, nil
	}
	for _, target := range targets {
		report := filepath.Join(path, ".moon", "cache", "runReport.json")
		if err := os.Remove(report); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		_, runErr := workspace.command(ctx, nil, nil, "moon", "run", target)
		failed, err := readRecheckTarget(report, target)
		if err != nil {
			return nil, fmt.Errorf("read target report: %w", err)
		}
		passed, err = appendRecheckPassed(passed, target, failed, runErr)
		if err != nil {
			return nil, err
		}
	}
	if len(gates) > 0 {
		binary := filepath.Join(dir, "gate-runner")
		if _, err := workspace.command(ctx, nil, nil, "go", "build", "-o", binary, "./tools/gate-runner"); err != nil {
			return nil, err
		}
		for _, gate := range gates {
			report := filepath.Join(dir, "gate-report.json")
			if err := os.Remove(report); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			_, runErr := workspace.command(ctx, []string{"RJC_GATE_REPORT=" + report}, nil, binary, "--only", gate, "--full")
			failed, err := readRecheckGate(report, gate)
			if err != nil {
				return nil, err
			}
			passed, err = appendRecheckPassed(passed, gate, failed, runErr)
			if err != nil {
				return nil, err
			}
		}
	}
	return passed, nil
}

func appendRecheckPassed(passed []string, name string, failed bool, runErr error) ([]string, error) {
	if err := recheckExit(runErr); err != nil {
		return nil, err
	}
	if !failed {
		if runErr != nil {
			return nil, runErr
		}
		passed = append(passed, name)
	}
	return passed, nil
}

// recheckInherited runs an inherited head's own increment, lowerTip..upperTip, with the hook config at upperTip.
// The lower head's failed run is only evidence for its failed targets. A failure outside those targets refuses
// the recheck, because it would be a failure of this head's own change.
func (r *repository) recheckInherited(ctx context.Context, rec HookRecord) (passed []string, resultErr error) {
	lowerTip := rec.CoveredBy
	dir, err := os.MkdirTemp("", "rjc-main-red-")
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(dir)) }()
	path := filepath.Join(dir, "workspace")
	name := filepath.Base(dir)
	if _, err = r.jj(ctx, "workspace", "add", "--name", name, "--revision", rec.Head, path); err != nil {
		return nil, err
	}
	defer func() {
		_, cleanupErr := r.jj(context.WithoutCancel(ctx), "workspace", "forget", name)
		resultErr = errors.Join(resultErr, cleanupErr)
	}()
	workspace := *r
	workspace.root = path
	report := filepath.Join(dir, "moon.json")
	env := []string{"MOON_BASE=" + lowerTip, "MOON_HEAD=" + rec.Head, "RJC_MOON_REPORT=" + report, "RJC_MOON_TARGETS=", "HK_FILE=" + hkConfigName}
	_, runErr := workspace.command(ctx, env, nil, "hk", "run", "pre-push", "--from-ref", lowerTip, "--to-ref", rec.Head)
	if err := recheckExit(runErr); err != nil {
		return nil, err
	}
	if runErr == nil {
		return nil, refusal("ready", "not-red-on-main", "the head's own increment passes the hook, so it is not red on main", "run hooks normally on this head to refresh its evidence")
	}
	failed, err := readMoonReport(report, nil, nil)
	if err != nil {
		return nil, toolFailure("ready", err)
	}
	return nil, acceptInheritedFailures(failed, rec.FailedTargets)
}

// acceptInheritedFailures decides an inherited recheck from the upper head's failing targets. A hook that failed
// with no target report is never accepted. A failing target is accepted only if the lower head's failed run named
// it. Any other failure would be a failure of this head's own change.
func acceptInheritedFailures(failed, lowerTargets []string) error {
	if len(failed) == 0 {
		return refusal("ready", "not-red-on-main", "upper head hook failed with no failing target in the report", "fix the head")
	}
	accepted := map[string]bool{}
	for _, target := range lowerTargets {
		accepted[target] = true
	}
	for _, target := range failed {
		if !accepted[target] {
			return refusal("ready", "not-red-on-main", "upper head fails outside the lower failed targets: "+target, "fix the head")
		}
	}
	return nil
}

func readRecheckTarget(path, target string) (bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // Report is in the private temporary directory.
	if err != nil {
		return false, err
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
		return false, errors.New("invalid moon report")
	}
	found, failed := false, false
	for _, action := range report.Actions {
		if action.Node.Action != "run-task" || action.Node.Params.Target != target {
			continue
		}
		found = true
		switch action.Status {
		case "failed":
			failed = true
		case "passed", "cached", "cached-from-remote":
		default:
			return false, fmt.Errorf("target %s has inconclusive status %q", target, action.Status)
		}
	}
	if !found {
		return false, fmt.Errorf("moon report missing %s", target)
	}
	return failed, nil
}

func recheckExit(err error) error {
	if err == nil {
		return nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil
	}
	return err
}

func readRecheckGate(path, name string) (bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // Report is in the private temporary directory.
	if err != nil {
		return false, err
	}
	var gates []struct {
		Name   string `json:"name"`
		Exit   *int   `json:"exit"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(data, &gates); err != nil {
		return false, err
	}
	for _, gate := range gates {
		if gate.Name == name {
			if gate.Reason != "" {
				return false, fmt.Errorf("gate %s has inconclusive status: %s", name, gate.Reason)
			}
			if gate.Exit == nil {
				return false, fmt.Errorf("gate %s has missing status", name)
			}
			switch *gate.Exit {
			case 0:
				return false, nil
			case 1:
				return true, nil
			default:
				return false, fmt.Errorf("gate %s has inconclusive status %d", name, *gate.Exit)
			}
		}
	}
	return false, fmt.Errorf("gate report missing %s", name)
}
