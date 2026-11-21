package resticenv

import (
	"strings"
	"testing"
)

func TestCoverageEnvironment(t *testing.T) {
	t.Setenv("RESTIC_PASSWORD", "secret")
	t.Setenv("TMPDIR", "unsafe")
	t.Setenv("COVERAGE_KEEP", "yes")
	env := Filter("/safe")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "RESTIC_PASSWORD=") ||
		!strings.Contains(joined, "TMPDIR=/safe") ||
		!strings.Contains(joined, "COVERAGE_KEEP=yes") {
		t.Fatal("environment filter did not preserve its contract")
	}
}
