// SPDX-License-Identifier: MIT OR Apache-2.0

// Package stepoutput appends key=value lines to the step output file that the
// CI backend reads when the step ends. Writes do nothing when CI_STEP_OUTPUT is
// unset.
package stepoutput

import (
	"fmt"
	"io"
	"os"
)

// Write appends key=value to CI_STEP_OUTPUT when it is set. A failed append is
// logged to stderr and does not change the caller's result.
func Write(getenv func(string) string, stderr io.Writer, key, value string) {
	path := getenv("CI_STEP_OUTPUT")
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // The CI agent supplies the output path.
	if err == nil {
		_, err = fmt.Fprintf(file, "%s=%s\n", key, value)
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		// Discarded on purpose: stderr is the only sink left for this message, and the step's own result does not depend on it.
		_, _ = fmt.Fprintf(stderr, "step output: failed to append: %v\n", err)
	}
}
