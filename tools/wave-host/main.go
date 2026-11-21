package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec" //nolint:depguard // Resolve the caller's tmux and agent executables.
	"path/filepath"
	"strconv"
)

func status(path, host string) Status {
	state, err := Load(path)
	if errors.Is(err, os.ErrNotExist) {
		return Status{State: "stopped", Host: host}
	}
	if err != nil {
		return Status{State: "blocked", Host: host, Reason: err.Error()}
	}
	return Status{State: state.State, Host: state.Host, InstanceID: state.InstanceID, Reason: state.Reason}
}

func run(ctx context.Context, args []string, out, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "seat-start" || args[0] == "seat-stop") {
		return runSeat(ctx, args, out, stderr)
	}
	if len(args) > 0 && (args[0] == "start" || args[0] == "stop") {
		return runManager(ctx, args, out, stderr)
	}
	if len(args) > 0 && args[0] == "process-identity" {
		return runProcessIdentity(args, out, stderr)
	}
	if len(args) == 0 || args[0] != "status" {
		if _, err := fmt.Fprintln(stderr, "usage: wave-host status --json [--state path] | start [--state path] -- COMMAND... | stop [--state path] | process-identity --pid N"); err != nil {
			return 2
		}
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return 1
		}
		return 1
	}
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("state", filepath.Join(home, "agents/run/wave-host/state.json"), "state file")
	jsonOutput := flags.Bool("json", false, "output JSON")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if !*jsonOutput || flags.NArg() != 0 {
		if _, err := fmt.Fprintln(stderr, "usage: wave-host status --json [--state path]"); err != nil {
			return 2
		}
		return 2
	}
	host, err := os.Hostname()
	if err != nil {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return 1
		}
		return 1
	}
	if err := json.NewEncoder(out).Encode(status(*path, host)); err != nil {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return 1
		}
		return 1
	}
	return 0
}

func runSeat(ctx context.Context, args []string, out, stderr io.Writer) int {
	home, err := os.UserHomeDir()
	if err != nil {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return 1
		}
		return 1
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	name := flags.String("name", "", "seat name")
	socket := flags.String("socket", "", "seat socket")
	conf := flags.String("conf", "", "tmux configuration")
	cwd := flags.String("cwd", "", "working directory")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if *name == "" {
		if _, err := fmt.Fprintln(stderr, "missing seat name"); err != nil {
			return 1
		}
		return 2
	}
	h := seatHost{run: runCommand, identify: Identify, connect: connectSeat, children: Children, dir: filepath.Join(home, "agents/run/wave-host/seats"), domain: "gui/" + strconv.Itoa(os.Getuid()), timeout: seatTimeout}
	if args[0] == "seat-stop" {
		if flags.NArg() != 0 {
			if _, err := fmt.Fprintln(stderr, "unexpected arguments"); err != nil {
				return 1
			}
			return 2
		}
		err = h.stop(ctx, *name)
		if err != nil {
			if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
				return 1
			}
			return 1
		}
		return 0
	}
	if *socket == "" || *conf == "" || *cwd == "" || flags.NArg() == 0 {
		if _, err := fmt.Fprintln(stderr, "missing seat start arguments"); err != nil {
			return 1
		}
		return 2
	}
	var tmux string
	tmux, err = exec.LookPath("tmux")
	command := flags.Args()
	if err == nil {
		command[0], err = exec.LookPath(command[0])
	}
	if err == nil {
		var holder, agent int
		holder, agent, err = h.start(ctx, seatStartArgs{name: *name, socket: *socket, conf: *conf, cwd: *cwd, tmux: tmux, path: os.Getenv("PATH"), command: command})
		if err == nil {
			_, err = fmt.Fprintf(out, "%d %d\n", holder, agent)
		}
	}
	if err != nil {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return 1
		}
		return 1
	}
	return 0
}

func runManager(ctx context.Context, args []string, out, stderr io.Writer) int {
	home, err := os.UserHomeDir()
	if err != nil {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return 1
		}
		return 1
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("state", filepath.Join(home, "agents/run/wave-host/state.json"), "state file")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	m := newManagerHost(*path, os.Getuid())
	if args[0] == "stop" {
		if flags.NArg() != 0 {
			if _, err := fmt.Fprintln(stderr, "unexpected arguments"); err != nil {
				return 1
			}
			return 2
		}
		err = m.stop(ctx)
	} else {
		err = m.start(ctx, flags.Args())
	}
	if err != nil {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return 1
		}
		return 1
	}
	return 0
}

func main() { os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr)) }
