// Package seam is the boundary between the watchdog logic and the host: /proc, the filesystem,
// child commands and the clock. The sampler and the capture steps depend on it; tests fake it.
package seam

import (
	"context"
	"time"

	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/command"
)

// Config holds the paths the watchdog reads and writes.
type Config struct {
	ProcRoot       string
	RuntimeDir     string
	CaptureRoot    string
	RelaunchBinary string
	// User is the account that owns the zellij server; user commands run as it.
	User string
	// Session is the zellij session name whose server the watchdog samples.
	Session string
	// LogDirectory holds the zellij server logs the watchdog tails.
	LogDirectory string
	// JournalUnit is the user systemd unit whose last ten minutes of journal are captured.
	JournalUnit string
}

// UserLogEntry is one regular file in the zellij log directory.
type UserLogEntry struct {
	Name     string
	Modified float64
	Size     int64
}

// LogFile is a bounded tail of one zellij log file.
type LogFile struct {
	Name     string
	Contents []byte
}

// LogRead is the set of log tails read under the aggregate cap, and the per-file read errors.
type LogRead struct {
	Files  []LogFile
	Errors []string
}

// DirEntry is the stat of one capture path.
type DirEntry struct {
	Name        string
	IsDirectory bool
	IsFile      bool
}

// IO is the full host seam the watchdog uses.
type IO interface {
	Clock
	ServerView
	FileStore
	CommandRunner
	StateLog
}

// Clock supplies time and sleeping, so the loop and tests control time.
type Clock interface {
	Now() int64
	Sleep(ctx context.Context, d time.Duration) error
}

// ServerView reads the server's proc view and its identity.
type ServerView interface {
	Config() Config
	Identity() string
	ReadDir(path string) ([]string, error)
	ReadText(path string) (string, error)
	CopyMemory(pid string, start, end uint64, outputPath, identity string) error
}

// FileStore reads and writes capture files under the watchdog's own directories.
type FileStore interface {
	ReadUserLogs(ctx context.Context, maxBytes int64) (LogRead, error)
	WriteText(path, text string) error
	WriteBytes(path string, data []byte) error
	Stat(path string) (DirEntry, error)
	Mkdir(path string) error
	Remove(path string) error
}

// CommandRunner runs bounded child commands.
type CommandRunner interface {
	Run(ctx context.Context, name string, args []string, timeout time.Duration) (command.Result, error)
}

// StateLog records state transitions and log lines.
type StateLog interface {
	RecordUnavailableTransition(identity string) bool
	Log(level, message string)
}
