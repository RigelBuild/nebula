package seat

import (
	"context"
	"io/fs"
	"time"

	"github.com/RigelBuild/nebula/tools/wave-seat-holder/internal/pane"
	"github.com/RigelBuild/nebula/tools/wave-seat-holder/internal/socket"
)

// CommandResult is the outcome of a child process that ran to completion.
type CommandResult = pane.Result

// Files is the filesystem and socket access the holder uses.
type Files struct {
	Mkdir    func(path string, perm fs.FileMode) error
	Readdir  func(path string) ([]string, error)
	ReadText func(path string) (string, error)
	Connect  func(ctx context.Context, path string) bool
	// OpenSeatDir opens the seat directory and checks that it is a real directory owned by the holder's uid.
	OpenSeatDir func(path string) (SeatDir, error)
}

// SeatDir is an open seat directory. Every operation is relative to its file descriptor, never a path.
type SeatDir interface {
	// WriteFile replaces name inside the directory with data at mode 0600, refusing any link or non-regular file.
	WriteFile(name string, data []byte) error
	Close() error
}

// Deps is every outside effect the holder uses, so tests can replace each one.
type Deps struct {
	Platform string
	Getenv   socket.Environment
	Cwd      func() (string, error)
	Now      func() time.Time
	Sleep    func(ctx context.Context, d time.Duration) error
	Files    Files
	Which    func(command string) (string, bool)
	Spawn    func(ctx context.Context, command string, args []string, inherit bool) (CommandResult, error)
	Stdout   func(line string)
	Stderr   func(line string)
}
