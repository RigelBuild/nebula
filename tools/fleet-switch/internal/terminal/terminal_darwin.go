package terminal

import (
	"os"

	"golang.org/x/sys/unix"
)

// IsTerminal reports whether file is a terminal.
func IsTerminal(file *os.File) bool {
	_, err := unix.IoctlGetTermios(int(file.Fd()), unix.TIOCGETA)
	return err == nil
}
