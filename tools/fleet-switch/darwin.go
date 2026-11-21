package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// darwinSwitchCommand runs nh-switch on the darwin host from the store path copied there.
func darwinSwitchCommand(flakeRef string, fleetHost host) command {
	remote := fmt.Sprintf("zsh -lic 'nh-switch darwin \"%s\"'", flakeRef)
	return command{name: sshCommand, args: []string{"-t", fleetHost.name, remote}}
}

// storePathPattern admits one store path. The path is spliced into a remote shell command.
var storePathPattern = regexp.MustCompile(`^/nix/store/[0-9a-z]{32}-[A-Za-z0-9+._?=-]+$`)

// prefetchCommand fetches the pinned tree into the caller's store and prints its store path as JSON.
func prefetchCommand(treeRef string) command {
	return command{name: "nix", args: []string{"flake", "prefetch", "--json", treeRef}}
}

// copyCommand copies one store path to the darwin host over ssh-ng; ssh config supplies the user, as for ssh.
func copyCommand(fleetHost host, storePath string) command {
	return command{name: "nix", args: []string{"copy", "--to", "ssh-ng://" + fleetHost.target, storePath}}
}

// darwinStoreRef is the darwin host's flake inside the copied tree at storePath.
func darwinStoreRef(storePath string, fleetHost host) string {
	dir, attr, _ := strings.Cut(fleetHost.flake, "#")
	return "path:" + storePath + "?dir=" + dir + "#" + attr
}

// storePathFromPrefetch reads the storePath field of nix flake prefetch --json.
func storePathFromPrefetch(out []byte) (string, error) {
	var prefetched struct {
		StorePath string `json:"storePath"`
	}
	if err := json.Unmarshal(out, &prefetched); err != nil {
		return "", fmt.Errorf("parse nix flake prefetch output: %w", err)
	}
	if !storePathPattern.MatchString(prefetched.StorePath) {
		return "", fmt.Errorf("nix flake prefetch returned store path %q", prefetched.StorePath)
	}
	return prefetched.StorePath, nil
}

// switchDarwin fetches the pinned tree, copies it to the darwin host, then runs nh-switch from that store path.
// A failed step stops the host, so nh-switch never runs after a failed copy.
func switchDarwin(ctx context.Context, deps dependencies, src deploySource, fleetHost host, dryRun bool) (bool, error) {
	prefetch := prefetchCommand(src.treeRef())
	if dryRun {
		return false, printDarwinPlan(deps.stdout, prefetch, fleetHost)
	}
	var out bytes.Buffer
	if failed, err := runDarwinStep(ctx, deps, prefetch, &out, fleetHost, "fetch"); failed || err != nil {
		return failed, err
	}
	storePath, err := storePathFromPrefetch(out.Bytes())
	if err != nil {
		_, printErr := fmt.Fprintf(deps.stderr, "%s: %v\n", fleetHost.name, err)
		return true, printErr
	}
	if failed, err := runDarwinStep(ctx, deps, copyCommand(fleetHost, storePath), deps.stdout, fleetHost, "copy"); failed || err != nil {
		return failed, err
	}
	switchCmd := darwinSwitchCommand(darwinStoreRef(storePath, fleetHost), fleetHost)
	return runDarwinStep(ctx, deps, switchCmd, deps.stdout, fleetHost, "switch")
}

// printDarwinPlan prints the darwin host's steps; the store path is unknown until prefetch runs.
func printDarwinPlan(out io.Writer, prefetch command, fleetHost host) error {
	const placeholder = "<store-path>"
	for _, cmd := range []command{prefetch, copyCommand(fleetHost, placeholder), darwinSwitchCommand(darwinStoreRef(placeholder, fleetHost), fleetHost)} {
		if _, err := fmt.Fprintln(out, formatCommand(cmd)); err != nil {
			return err
		}
	}
	return nil
}

// runDarwinStep runs one darwin-host step. A failed step fails the host; cancellation is an error.
func runDarwinStep(ctx context.Context, deps dependencies, cmd command, stdout io.Writer, fleetHost host, step string) (bool, error) {
	err := deps.runner(ctx, cmd.name, cmd.args, stdout, deps.stderr)
	if err == nil {
		return false, nil
	}
	if ctx.Err() != nil {
		return false, fmt.Errorf("switch interrupted on %s during %s; remote state unknown: %w", fleetHost.name, step, ctx.Err())
	}
	_, printErr := fmt.Fprintf(deps.stderr, "%s: %s failed: %v\n", fleetHost.name, step, err)
	return true, printErr
}
