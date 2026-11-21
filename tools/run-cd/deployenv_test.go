package main

import (
	"strings"
	"testing"
)

// TestDeployEnvDropsCDTokenKeepsRest: the deploy child must not see the CD API
// token, but keeps the rest of the step environment it needs.
func TestDeployEnvDropsCDTokenKeepsRest(t *testing.T) {
	t.Setenv("WOODPECKER_CD_TOKEN", "cd-token-value")
	t.Setenv("CI_PIPELINE_NUMBER", "7")
	var sawNumber bool
	for _, kv := range deployEnv() {
		if strings.HasPrefix(kv, "WOODPECKER_CD_TOKEN=") {
			t.Fatalf("WOODPECKER_CD_TOKEN reached the deploy environment: %q", kv)
		}
		if strings.Contains(kv, "cd-token-value") {
			t.Fatalf("the deploy environment leaks the CD token value: %q", kv)
		}
		if kv == "CI_PIPELINE_NUMBER=7" {
			sawNumber = true
		}
	}
	if !sawNumber {
		t.Fatal("CI_PIPELINE_NUMBER was dropped; the deploy needs it")
	}
}
