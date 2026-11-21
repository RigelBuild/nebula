//go:build !linux && !darwin

package terminal

import "os"

// IsTerminal reports whether file is a terminal.
func IsTerminal(_ *os.File) bool { return false }
