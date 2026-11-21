// Package check reports the named pass/fail assertions the Alloy harnesses print.
package check

import (
	"fmt"
	"io"
	"log/slog"
)

// Check is one named assertion and whether it held.
type Check struct {
	Name string
	OK   bool
}

// Report prints every check, then a failure count when any check failed. It returns
// the process exit code: 0 when every check held, 1 otherwise.
func Report(stdout, stderr io.Writer, checks []Check) int {
	failed, err := writeChecks(stdout, checks)
	if err != nil {
		slog.Error("print checks", "err", err)
		return 1
	}
	if failed == 0 {
		return 0
	}
	if _, err := fmt.Fprintf(stderr, "%d check(s) failed\n", failed); err != nil {
		slog.Error("print failure count", "err", err)
	}
	return 1
}

// Usage writes the usage line and returns the process exit code for a bad argv.
func Usage(stderr io.Writer, usage string) int {
	if _, err := fmt.Fprintln(stderr, usage); err != nil {
		slog.Error("print usage", "err", err)
	}
	return 1
}

// writeChecks writes one line per check and returns how many failed.
func writeChecks(w io.Writer, checks []Check) (int, error) {
	failed := 0
	for _, c := range checks {
		status := "ok  "
		if !c.OK {
			status = "FAIL"
			failed++
		}
		if _, err := fmt.Fprintf(w, "%s %s\n", status, c.Name); err != nil {
			return failed, fmt.Errorf("write check line: %w", err)
		}
	}
	return failed, nil
}
