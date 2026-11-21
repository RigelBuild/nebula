package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/rawcap"
	"github.com/spf13/cobra"
)

// resticExitNoRepo is restic's exit status when the repository does not exist yet.
const resticExitNoRepo = 10

func newRawCapCmd() *cobra.Command {
	var repo, stateDir string
	var capBytes uint64
	command := &cobra.Command{
		Use:           "rawcap",
		Short:         "Refuse a pass when the repository holds at least the cap in raw bytes",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRawCap(cmd, repo, stateDir, capBytes)
		},
	}
	flags := command.Flags()
	flags.StringVar(&repo, "repo", "", "restic repository path or URL")
	flags.StringVar(&stateDir, "state-dir", "", "pass state directory holding the repository password")
	flags.Uint64Var(&capBytes, "cap-bytes", 0, "refuse when raw-data bytes are at or above this")
	for _, name := range []string{"repo", "state-dir", "cap-bytes"} {
		if err := command.MarkFlagRequired(name); err != nil {
			return command
		}
	}
	return command
}

// runRawCap reads restic's raw-data size and returns rawcap's refusal as the command error.
// A first-ever run (no password yet, or no repository) holds 0 bytes; every other failure refuses.
func runRawCap(cmd *cobra.Command, repo, stateDir string, capBytes uint64) error {
	passwordFile := filepath.Join(stateDir, "password")
	if _, err := os.ReadFile(passwordFile); err != nil { //nolint:gosec // Password is in the operator-supplied state directory.
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read password: %w", err)
	}
	restic, err := resticCommand(coverageOptions{repo: repo, passwordFile: passwordFile}, "stats", "--mode", "raw-data", "--json", "--no-lock")
	if err != nil {
		return err
	}
	var stdout bytes.Buffer
	restic.Stdout = &stdout
	if err := restic.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == resticExitNoRepo {
			return nil
		}
		return fmt.Errorf("restic stats: %w", err)
	}
	return rawcap.Check(stdout.Bytes(), capBytes)
}
