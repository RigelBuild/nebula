// Package main runs one deploy command for the CI step output. It records the
// moon target, then "started", then "completed" only when the command exits 0.
// The usage is `deploy-output --target <target> -- <command...>`.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec" //nolint:depguard // deploy-output runs the deploy command directly, without a shell.
	"strings"
	"syscall"

	"github.com/RigelBuild/nebula/tools/go-lib/stepoutput"
)

const usage = "usage: deploy-output --target <target> -- <command...>"

// Exit codes. A command that exits nonzero keeps its own code; a signal death
// reports 128 plus the signal number, as the shell does.
const (
	usageError  = 2
	spawnFailed = 127
	waitFailed  = 1
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run is the testable body of main. It returns the process exit code.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	target, command, ok := parseArgs(args)
	if !ok {
		return reportExit(stderr, usageError, usage)
	}
	stepoutput.Write(getenv, stderr, "deploy.target", target)
	stepoutput.Write(getenv, stderr, "deploy.state", "started")
	cmd := exec.Command(command[0], command[1:]...) //nolint:gosec // The argv is the step's own command; no shell.
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return reportExit(stderr, spawnFailed, "deploy-output: spawn failed: "+strings.ReplaceAll(err.Error(), "\n", " "))
	}
	err := cmd.Wait()
	if err == nil {
		stepoutput.Write(getenv, stderr, "deploy.state", "completed")
		return 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return reportExit(stderr, waitFailed, fmt.Sprintf("deploy-output: wait failed: %v", err))
	}
	return exitCode(exitErr)
}

// reportExit writes msg as one stderr line and returns code. The write error is
// discarded on purpose: stderr is the only sink, and the exit code is what the
// step reads.
func reportExit(stderr io.Writer, code int, msg string) int {
	_, _ = fmt.Fprintln(stderr, msg)
	return code
}

// parseArgs reads `--target <target> -- <command...>`. The target must be
// non-empty, and the command must have at least one word.
func parseArgs(args []string) (string, []string, bool) {
	if len(args) < 4 || args[0] != "--target" || args[1] == "" || args[2] != "--" {
		return "", nil, false
	}
	return args[1], args[3:], true
}

// exitCode is the step exit code for a command that ran and then exited.
func exitCode(exitErr *exec.ExitError) int {
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return exitErr.ExitCode()
}
