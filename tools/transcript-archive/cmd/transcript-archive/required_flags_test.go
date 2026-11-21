package main

import (
	"path/filepath"
	"testing"
)

// TestPassRequiresConfigAndEscrowRecipient checks that pass refuses to run when a
// deployment input is missing. Nothing is baked in, so each case must fail before
// the state directory or repository is touched.
func TestPassRequiresConfigAndEscrowRecipient(t *testing.T) {
	t.Parallel()
	base := []string{
		"--host", "example-host",
		"--state-dir", filepath.Join(t.TempDir(), "state"),
		"--repo", filepath.Join(t.TempDir(), "repo"),
		"--escrow-out", filepath.Join(t.TempDir(), "escrow.age"),
	}
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing config", args: append([]string{"--escrow-recipient", "age1example"}, base...)},
		{name: "missing escrow recipient", args: append([]string{"--config", fixtureConfig}, base...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			command := newPassCmd()
			command.SilenceUsage = true
			command.SilenceErrors = true
			command.SetArgs(test.args)
			if err := command.ExecuteContext(t.Context()); err == nil {
				t.Fatalf("pass accepted %s", test.name)
			}
		})
	}
}
