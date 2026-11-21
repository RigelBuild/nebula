package main

import (
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

// TestPassRequiresConfigAndEscrowRecipient checks that pass refuses to run when a
// deployment input is missing. Nothing is baked in, so each case must fail before
// the state directory or repository is touched.
func TestPassRequiresConfigAndEscrowRecipient(t *testing.T) {
	t.Parallel()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	recipient := identity.Recipient().String()
	base := []string{
		"--host", "example-host",
		"--state-dir", filepath.Join(t.TempDir(), "state"),
		"--repo", filepath.Join(t.TempDir(), "repo"),
		"--escrow-out", filepath.Join(t.TempDir(), "escrow.age"),
	}
	tests := []struct {
		name        string
		args        []string
		missingFlag string
	}{
		{
			name:        "missing config",
			args:        append([]string{"--escrow-recipient", recipient}, base...),
			missingFlag: "config",
		},
		{
			name:        "missing escrow recipient",
			args:        append([]string{"--config", fixtureConfig}, base...),
			missingFlag: "escrow-recipient",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			command := newPassCmd()
			command.SilenceUsage = true
			command.SilenceErrors = true
			command.SetArgs(test.args)
			runErr := command.ExecuteContext(t.Context())
			if runErr == nil {
				t.Fatalf("pass accepted %s", test.name)
			}
			msg := runErr.Error()
			if !strings.HasPrefix(msg, "pass requires ") || !strings.Contains(msg, test.missingFlag) {
				t.Fatalf("pass error = %q, want the required-flag error naming %s", msg, test.missingFlag)
			}
		})
	}
}

// TestPassRequiredFlagsHaveNoDefault pins the empty defaults: a default would
// let pass start without the input that the required-flag check demands.
func TestPassRequiredFlagsHaveNoDefault(t *testing.T) {
	t.Parallel()
	command := newPassCmd()
	for _, name := range []string{"config", "escrow-recipient"} {
		flag := command.Flags().Lookup(name)
		if flag == nil {
			t.Fatalf("pass has no --%s flag", name)
		}
		if flag.DefValue != "" {
			t.Fatalf("--%s default = %q, want empty", name, flag.DefValue)
		}
	}
}
