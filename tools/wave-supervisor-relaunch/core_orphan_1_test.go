package main

import (
	"testing"
)

func testCoreTriggerAndSupervisorStateOrphanWorkersUnderALiveSupervisor(t *testing.T) {
	t.Helper()
	lanes := map[string]struct{}{"accounts": {}, "runners": {}}
	if got := classifySupervisor(testPSEnvelope(`{"name":"foreman","mesh":"idle","meshView":"current"}`), "foreman/foreman agent ○ idle\nrunners/rigel-runners-2 agent ○ idle\naccounts/worker agent ○ idle", "foreman", "agent", lanes); got.Kind != "live" {
		t.Fatalf("live = %#v", got)
	}
	if got := classifySupervisor(testPSEnvelope(""), "accounts/worker agent ○ idle", "foreman", "agent", lanes); got.Kind != "absent" {
		t.Fatalf("workers without supervisor = %#v", got)
	}
	if got := classifySupervisor(testPSEnvelope(`{"name":"foreman","mesh":"starting"}`), "accounts/worker agent ○ idle", "foreman", "agent", lanes); got.Kind != "starting" {
		t.Fatalf("starting = %#v", got)
	}
	if got := classifySupervisor(testPSEnvelope("{"), "", "foreman", "agent", lanes); got.Kind != "invalid" {
		t.Fatalf("malformed = %#v", got)
	}
}
