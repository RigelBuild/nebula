// SPDX-License-Identifier: MIT OR Apache-2.0

package relaunch

import (
	"context"
	"time"
)

type ReadinessKind string

const (
	ReadinessReady   ReadinessKind = "ready"
	ReadinessWaiting ReadinessKind = "waiting"
	ReadinessBlocked ReadinessKind = "blocked"
)

const (
	statusUnavailable = "status-unavailable"
	stateRetrying     = "retrying"
)

type Readiness struct {
	Kind ReadinessKind
	Why  string
}

// ManagerReady classifies manager health without checking per-alias failures.
func ManagerReady(status any) Readiness {
	return ClassifyStatus(status, "")
}

func object(value any) map[string]any {
	row, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return row
}

func ClassifyStatus(status any, name string) Readiness {
	envelope := object(status)
	data := object(envelope["staticReconciliation"])
	if data == nil {
		data = envelope
	}
	if data == nil {
		return Readiness{Kind: ReadinessWaiting, Why: statusUnavailable}
	}
	state, _ := data["state"].(string)
	lastSweep := object(data["lastSweep"])
	if value, exists := lastSweep["error"]; exists && value != nil && value != "" {
		return Readiness{Kind: ReadinessBlocked, Why: "sweep-error"}
	}
	if state == "failed" {
		return Readiness{Kind: ReadinessBlocked, Why: "manager-failed"}
	}
	if state == "running" || state == stateRetrying {
		return Readiness{Kind: ReadinessWaiting, Why: "manager-" + state}
	}
	completed, exists := lastSweep["completedAt"]
	if !exists || completed == nil || completed == "" {
		return Readiness{Kind: ReadinessWaiting, Why: "sweep-incomplete"}
	}
	failures, _ := data["failures"].([]any)
	for _, item := range failures {
		failure := object(item)
		if failure == nil || failure["alias"] != name || name == "" {
			continue
		}
		phase, _ := failure["phase"].(string)
		disposition, _ := failure["disposition"].(string)
		switch {
		case phase == "retry-scheduled", phase == stateRetrying, disposition == "retry-scheduled", disposition == stateRetrying:
			return Readiness{Kind: ReadinessWaiting, Why: "supervisor-retrying"}
		case phase == "refused", phase == "refused-foreign", phase == "retry-exhausted", disposition == "refused", disposition == "refused-foreign", disposition == "retry-exhausted":
			return Readiness{Kind: ReadinessBlocked, Why: "supervisor-refused"}
		}
	}
	return Readiness{Kind: ReadinessReady}
}

type ReadyDeps struct {
	Status   func(context.Context) (any, error)
	Now      func() time.Time
	Sleep    func(context.Context, time.Duration) error
	Deadline time.Duration
	Poll     time.Duration
	Classify func(any) Readiness
}

func WaitReady(ctx context.Context, deps ReadyDeps) Readiness {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	sleep := deps.Sleep
	if sleep == nil {
		sleep = func(ctx context.Context, duration time.Duration) error {
			timer := time.NewTimer(duration)
			defer timer.Stop()
			select {
			case <-timer.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	classify := deps.Classify
	if classify == nil {
		classify = func(status any) Readiness { return ClassifyStatus(status, "foreman") }
	}
	started := now()
	last := Readiness{Kind: ReadinessWaiting, Why: statusUnavailable}
	for {
		if err := ctx.Err(); err != nil {
			return last
		}
		status, err := deps.Status(ctx)
		if err != nil {
			last = Readiness{Kind: ReadinessWaiting, Why: statusUnavailable}
		} else {
			last = classify(status)
		}
		if last.Kind != ReadinessWaiting {
			return last
		}
		elapsed := now().Sub(started)
		if elapsed >= deps.Deadline {
			return last
		}
		delay := min(deps.Poll, deps.Deadline-elapsed)
		if err := sleep(ctx, delay); err != nil {
			return last
		}
	}
}
