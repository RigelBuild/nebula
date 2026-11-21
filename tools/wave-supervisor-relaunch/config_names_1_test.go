package main

import (
	"context"
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
