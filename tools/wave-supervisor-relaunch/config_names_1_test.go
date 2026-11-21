package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RigelBuild/nebula/tools/go-lib/cotalreach"
)

// renamedConfig gives the deployment names that differ from the old literals. A path that still
// hard-codes a literal keys on the wrong unit or runs the wrong command and fails this test.
func renamedConfig() Config {
	cfg := testConfig()
	cfg.CotalManagerUnit = "ops-manager.service"
	cfg.SeatCLI = "seatctl"
	return cfg
}

func renamedTrigger(invocation string) Trigger {
	return Trigger{
		BootID:       "boot-fixture",
		ZellijServer: "server-fixture",
		ManagerUnit:  "ops-manager.service",
		Units: []UnitFact{
			{Unit: "ops-manager.service", InvocationID: invocation, ActiveState: "active"},
		},
	}
}

func TestConfiguredManagerUnitKeysTheFingerprint(t *testing.T) {
	got := fingerprint(renamedTrigger("inv-1"))
	want := "boot-fixture:inv-1:devbox=server-fixture"
	if got != want {
		t.Fatalf("fingerprint() = %q, want %q: the manager unit must come from config, not a literal", got, want)
	}
	// A literal cotal-manager.service would find no fact here and key on an empty invocation.
	if strings.Contains(got, "boot-fixture::") {
		t.Fatalf("fingerprint() = %q keyed on an empty manager invocation", got)
	}
}

func TestConfiguredManagerUnitUnchangedIsNotDetected(t *testing.T) {
	deps := runDeps{Config: renamedConfig()}
	before := renamedTrigger("inv-1")
	deps.Trigger = func(context.Context) (Trigger, error) { return renamedTrigger("inv-1"), nil }
	changed, err := managerChanged(context.Background(), deps, before)
	if err != nil || changed {
		t.Fatalf("managerChanged() = %t, %v; want false when the configured unit's invocation is unchanged", changed, err)
	}
}

func TestConfiguredManagerUnitDeactivatingSkipsAsStopping(t *testing.T) {
	h := newRunHarness()
	h.deps.Config.CotalManagerUnit = "ops-manager.service"
	h.deps.Trigger = func(context.Context) (Trigger, error) {
		return Trigger{BootID: "boot", ManagerUnit: "ops-manager.service", Units: []UnitFact{{Unit: "ops-manager.service", InvocationID: "manager", ActiveState: "deactivating"}}}, nil
	}
	if got := runSupervisor(t.Context(), h.deps); got.Reason != "manager-stopping" {
		t.Fatalf("run=%#v, want manager-stopping for the configured unit", got)
	}
	if h.hasCall("ps") || h.hasCall("endpoints") || h.countPrefix("spawn:") != 0 {
		t.Fatalf("manager-stop run classified or spawned: %v", h.calls)
	}
}

func TestConfiguredManagerUnitChangeIsDetected(t *testing.T) {
	deps := runDeps{Config: renamedConfig()}
	before := renamedTrigger("inv-1")
	deps.Trigger = func(context.Context) (Trigger, error) { return renamedTrigger("inv-2"), nil }
	changed, err := managerChanged(context.Background(), deps, before)
	if err != nil || !changed {
		t.Fatalf("managerChanged() = %t, %v; want true when the configured unit's invocation changes", changed, err)
	}
}

func TestConfiguredSeatCLIRunsTheRestart(t *testing.T) {
	var argv []string
	deps := runDeps{
		Config: renamedConfig(),
		Runner: func(_ context.Context, args []string, _ string) (cotalreach.CommandResult, error) {
			argv = args
			return cotalreach.CommandResult{}, nil
		},
	}
	seatDeps := seatStepDeps(context.Background(), deps, &strings.Builder{})
	if _, err := seatDeps.Restart(context.Background(), "alpha"); err != nil {
		t.Fatalf("Restart() error = %v", err)
	}
	want := []string{"seatctl", "restart", "alpha"}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Fatalf("Restart() ran %q, want %q: the seat CLI must come from config", argv, want)
	}
}

// TestLiveZellijSessionTable pins the configured session check. The viewer must be live under its
// own name; a live devbox, an EXITED viewer, a failed list, or a non-zero exit all read as not live.
func TestLiveZellijSessionTable(t *testing.T) {
	cases := []struct {
		name   string
		result cotalreach.CommandResult
		err    error
		want   string
	}{
		{name: "viewer live", result: cotalreach.CommandResult{Stdout: "viewer [Created 1h ago]\n"}, want: "viewer"},
		{name: "only devbox listed", result: cotalreach.CommandResult{Stdout: "devbox [Created 1h ago]\n"}, want: ""},
		{name: "viewer exited", result: cotalreach.CommandResult{Stdout: "viewer [EXITED - attach to resurrect]\n"}, want: ""},
		{name: "non-zero exit", result: cotalreach.CommandResult{Stdout: "viewer [Created 1h ago]\n", ExitCode: 1}, want: ""},
		{name: "runner error", err: errors.New("no zellij"), want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := func(context.Context, []string, string) (cotalreach.CommandResult, error) {
				return tc.result, tc.err
			}
			got, err := liveZellijSession(context.Background(), runner, "viewer")
			if tc.err != nil {
				if err == nil {
					t.Fatalf("liveZellijSession() error = nil, want the runner error")
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("liveZellijSession() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
