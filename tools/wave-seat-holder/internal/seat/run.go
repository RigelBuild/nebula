// Package seat starts, attaches to, interrupts and lists the seats one holder serves.
package seat

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/RigelBuild/nebula/tools/wave-seat-holder/internal/pane"
	"github.com/RigelBuild/nebula/tools/wave-seat-holder/internal/socket"
)

const (
	usageRoot      = "Usage: wave-seat-holder <start|attach|interrupt|list> [options]\n"
	usageStart     = "Usage: wave-seat-holder start --name <name> -- <command> [args...]\n"
	usageAttach    = "Usage: wave-seat-holder attach --name <name> [--read-only]\n"
	usageInterrupt = "Usage: wave-seat-holder interrupt --name <name>\n"
	usageList      = "Usage: wave-seat-holder list --json\n"
)

// Run dispatches one holder command and returns its exit code.
func Run(ctx context.Context, args []string, deps Deps) int {
	command := ""
	if len(args) > 0 {
		command = args[0]
	}
	switch command {
	case "start":
		return runStart(ctx, args, deps)
	case "attach":
		return runAttach(ctx, args, deps)
	case "interrupt":
		return runInterrupt(ctx, args, deps)
	case "list":
		return listSeats(ctx, args, deps)
	default:
		deps.Stderr(usageRoot)
		return 2
	}
}

func runStart(ctx context.Context, args []string, deps Deps) int {
	name, ok := readName(args, "start")
	if !ok || len(args) < 5 || args[3] != "--" {
		deps.Stderr(usageStart)
		return 2
	}
	return startSeat(ctx, name, args[4:], deps)
}

func runAttach(ctx context.Context, args []string, deps Deps) int {
	name, ok := readName(args, "attach")
	var options []string
	if len(args) > 3 {
		options = args[3:]
	}
	valid := ok && (len(options) == 0 || (len(options) == 1 && options[0] == "--read-only"))
	if !valid {
		deps.Stderr(usageAttach)
		return 2
	}
	return attachSeat(ctx, name, len(options) == 1, deps)
}

func runInterrupt(ctx context.Context, args []string, deps Deps) int {
	name, ok := readName(args, "interrupt")
	if !ok || len(args) != 3 {
		deps.Stderr(usageInterrupt)
		return 2
	}
	return interruptSeat(ctx, name, deps)
}

// readName returns the value after "<command> --name" when args have that shape.
func readName(args []string, command string) (string, bool) {
	if len(args) < 3 || args[0] != command || args[1] != "--name" {
		return "", false
	}
	return args[2], true
}

// seatSocketFor resolves the socket path for name in the configured socket directory.
func seatSocketFor(name string, deps Deps) (string, error) {
	directory, err := socket.Directory(deps.Getenv)
	if err != nil {
		return "", err
	}
	return socket.Path(name, directory, deps.Platform)
}

func attachSeat(ctx context.Context, name string, readOnly bool, deps Deps) int {
	socketPath, err := seatSocketFor(name, deps)
	if err != nil {
		return reportFailure(deps, err.Error())
	}
	args := []string{"-S", socketPath, "attach"}
	if readOnly {
		args = append(args, "-r")
	}
	args = append(args, "-t", sessionName)
	result, err := deps.Spawn(ctx, tmuxCommand, args, true)
	if err != nil {
		return reportFailure(deps, err.Error())
	}
	return result.Code
}

func interruptSeat(ctx context.Context, name string, deps Deps) int {
	socketPath, err := seatSocketFor(name, deps)
	if err != nil {
		return reportFailure(deps, err.Error())
	}
	args := []string{"-S", socketPath, "send-keys", "-t", sessionName, "C-c"}
	result, err := deps.Spawn(ctx, tmuxCommand, args, false)
	if err != nil {
		return reportFailure(deps, err.Error())
	}
	if result.Code != 0 {
		return reportFailure(deps, "tmux interrupt failed: "+failureDetail(result))
	}
	return 0
}

func listSeats(ctx context.Context, args []string, deps Deps) int {
	if len(args) != 2 || args[1] != "--json" {
		deps.Stderr(usageList)
		return 2
	}
	directory, err := socket.Directory(deps.Getenv)
	if err != nil {
		return reportFailure(deps, err.Error())
	}
	entries, err := deps.Files.Readdir(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		return reportFailure(deps, err.Error())
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry, ".sock") {
			continue
		}
		name := strings.TrimSuffix(entry, ".sock")
		socketPath, err := socket.Path(name, directory, deps.Platform)
		if err != nil {
			// An invalid or over-long name cannot be a seat socket, so it is skipped.
			continue
		}
		if deps.Files.Connect(ctx, socketPath) {
			quoted, quoteErr := socket.QuoteJSON(name)
			if quoteErr != nil {
				return reportFailure(deps, quoteErr.Error())
			}
			deps.Stdout(fmt.Sprintf(`{"name":%s,"status":"running"}`+"\n", quoted))
		}
	}
	return 0
}

// reportFailure writes the reason to stderr and returns the failure exit code.
func reportFailure(deps Deps, reason string) int {
	deps.Stderr("wave-seat-holder: " + reason + "\n")
	return 1
}

// failureDetail is the trimmed stderr of a failed command, or its exit code when stderr is blank.
func failureDetail(result CommandResult) string {
	return pane.Detail(result)
}
