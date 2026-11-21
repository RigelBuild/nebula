// SPDX-License-Identifier: MIT OR Apache-2.0

// Package cotalreach implements the Cotal presence and reachability checks used by spawn-agent.
package cotalreach

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec" //nolint:depguard // Cotal uses structured argv and context; a shell would change argument boundaries.
	"strings"
	"time"
)

const (
	VIEW_FAILED   = 3
	SPAWN_REFUSED = 4
	NOT_REACHABLE = 5
	SEAT_LIVE     = 6
	space         = "main"
)

// CommandResult is the captured result of one Cotal command.
type CommandResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// CommandRunner runs Cotal arguments without a shell.
type CommandRunner func(context.Context, []string) (CommandResult, error)

func runCommand(ctx context.Context, args []string) (CommandResult, error) {
	command := exec.CommandContext(ctx, "cotal", args...) //nolint:gosec // Cotal is fixed; args preserve caller argv without shell parsing.
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return CommandResult{ExitCode: exitErr.ExitCode(), Stdout: stdout.String(), Stderr: stderr.String()}, nil
		}
		return CommandResult{}, fmt.Errorf("run cotal: %w", err)
	}
	return CommandResult{Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

// BuildPsArgs requests machine-readable presence for the main space.
func BuildPsArgs() []string {
	return []string{"ps", "--space", space, "--json"}
}

// BuildDmArgs builds the reachability message for one agent.
func BuildDmArgs(name, text string) []string {
	return []string{"send", "--to", name, text, "--space", space}
}

// ParsePsItems validates a Cotal ps envelope and returns its kind and rows.
func ParsePsItems(output string) (string, []map[string]json.RawMessage, bool) {
	var envelope struct {
		SchemaVersion int    `json:"schemaVersion"`
		Command       string `json:"command"`
		Outcome       string `json:"outcome"`
		Result        struct {
			Kind  string                       `json:"kind"`
			Items []map[string]json.RawMessage `json:"items"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(output), &envelope) != nil || envelope.SchemaVersion != 1 || envelope.Command != "ps" || (envelope.Outcome != "ok" && envelope.Outcome != "partial") || (envelope.Result.Kind != "seats" && envelope.Result.Kind != "presence") || envelope.Result.Items == nil {
		return "", nil, false
	}
	return envelope.Result.Kind, envelope.Result.Items, true
}

// ParsePresentAgentNames returns exact names with a current, live mesh state.
func ParsePresentAgentNames(output string) map[string]struct{} {
	names := map[string]struct{}{}
	kind, rows, valid := ParsePsItems(output)
	if !valid {
		return names
	}
	for _, row := range rows {
		name, ok := stringField(row, "name")
		if !ok {
			continue
		}
		field := "mesh"
		if kind == "presence" {
			field = "status"
		}
		state, ok := stringField(row, field)
		if ok && presentMeshState(state) && (kind != "seats" || currentMeshView(row)) {
			names[name] = struct{}{}
		}
	}
	return names
}

func stringField(row map[string]json.RawMessage, field string) (string, bool) {
	raw := bytes.TrimSpace(row[field])
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func presentMeshState(mesh string) bool {
	switch mesh {
	case "idle", "waiting", "working":
		return true
	default:
		return false
	}
}

func currentMeshView(row map[string]json.RawMessage) bool {
	raw, exists := row["meshView"]
	if !exists {
		return true
	}
	view, ok := stringField(map[string]json.RawMessage{"meshView": raw}, "meshView")
	return ok && view == "current"
}

// RunCotal executes a Cotal command and reports nonzero exits with stderr.
func RunCotal(ctx context.Context, args []string, runner CommandRunner) (CommandResult, error) {
	if runner == nil {
		runner = runCommand
	}
	result, err := runner(ctx, args)
	if err != nil {
		return CommandResult{}, fmt.Errorf("run cotal command: %w", err)
	}
	if result.ExitCode != 0 {
		command := ""
		if len(args) > 0 {
			command = args[0]
		}
		return CommandResult{}, fmt.Errorf("cotal %s exited %d: %s", command, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return result, nil
}

// PresenceOptions configures polling for an agent in Cotal's mesh.
type PresenceOptions struct {
	Timeout time.Duration
	Poll    time.Duration
	ReadPS  func(context.Context) (string, error)
	Sleep   func(context.Context, time.Duration) error
	Now     func() time.Time
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// WaitForAgentPresence polls until the exact agent appears or the deadline expires.
func WaitForAgentPresence(ctx context.Context, name string, options PresenceOptions) error {
	if options.ReadPS == nil {
		return errors.New("read Cotal presence: missing reader")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Sleep == nil {
		options.Sleep = sleep
	}
	deadline := options.Now().Add(options.Timeout)
	for {
		output, err := options.ReadPS(ctx)
		if err != nil {
			return err
		}
		if _, ok := ParsePresentAgentNames(output)[name]; ok {
			return nil
		}
		if !options.Now().Before(deadline) {
			break
		}
		remaining := deadline.Sub(options.Now())
		if err := options.Sleep(ctx, min(options.Poll, remaining)); err != nil {
			return err
		}
		if !options.Now().Before(deadline) {
			break
		}
	}
	return fmt.Errorf("agent %s did not appear in cotal ps --space %s", name, space)
}

// VerifyOptions configures the presence polling used before the DM ping.
type VerifyOptions struct {
	Timeout time.Duration
	Poll    time.Duration
	Sleep   func(context.Context, time.Duration) error
}

// VerifyAgentReachability waits for presence, then sends a direct-message ping.
func VerifyAgentReachability(ctx context.Context, name string, runner CommandRunner, options VerifyOptions) error {
	if options.Timeout == 0 {
		options.Timeout = 120 * time.Second
	}
	if options.Poll == 0 {
		options.Poll = 500 * time.Millisecond
	}
	err := WaitForAgentPresence(ctx, name, PresenceOptions{
		Timeout: options.Timeout,
		Poll:    options.Poll,
		Sleep:   options.Sleep,
		ReadPS: func(ctx context.Context) (string, error) {
			result, err := RunCotal(ctx, BuildPsArgs(), runner)
			return result.Stdout, err
		},
	})
	if err != nil {
		return err
	}
	ping := "spawn-agent reachability check " + name
	_, err = RunCotal(ctx, BuildDmArgs(name, ping), runner)
	return err
}
