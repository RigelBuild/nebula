// Command node-removal reconciles node removal from the desired-nodes ConfigMap.
// It exits 0 when the run completes and 1 when the desired set is invalid or the run aborts.
package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/RigelBuild/nebula/tools/node-removal/internal/kube"
	"github.com/RigelBuild/nebula/tools/node-removal/internal/reconcile"
)

// desiredDir is the mount point of the desired-nodes ConfigMap.
const desiredDir = "/etc/desired-nodes"

// desiredKeys are the ConfigMap keys, read in a fixed order so a missing key aborts the same way each run.
var desiredKeys = []string{
	"members",
	"retiring",
	"renamed",
	"mode",
	"min-nodes",
	"not-ready-grace-seconds",
}

func main() {
	os.Exit(run(os.Stdout, readDesiredFiles, os.Getenv))
}

// readDesiredFiles reads every ConfigMap key. A missing key is an error, so the run aborts before any write.
func readDesiredFiles() (map[string]string, error) {
	files := make(map[string]string, len(desiredKeys))
	for _, key := range desiredKeys {
		raw, err := os.ReadFile(desiredDir + "/" + key) //nolint:gosec // G304: fixed ConfigMap mount and fixed key list
		if err != nil {
			return nil, fmt.Errorf("reading desired key %s: %w", key, err)
		}
		files[key] = string(raw)
	}
	return files, nil
}

// abort writes the single JSON abort line to stdout and returns exit code 1.
func abort(stdout io.Writer, mode string, reason error) int {
	encoded, err := reconcile.EncodeLogLine(map[string]string{"event": "abort", "mode": mode, "reason": reason.Error()})
	if err != nil {
		return 1
	}
	_, _ = fmt.Fprintln(stdout, string(encoded)) // Exit 1 already reports the failure, so a failed write adds nothing.
	return 1
}

// run is the testable body: it reads the desired set, reconciles, and returns the exit code.
func run(stdout io.Writer, readFiles func() (map[string]string, error), getenv func(string) string) int {
	files, err := readFiles()
	if err != nil {
		return abort(stdout, string(reconcile.ModeDryRun), err)
	}
	initial, err := reconcile.ParseDesired(files)
	if err != nil {
		return abort(stdout, string(reconcile.ModeDryRun), err)
	}
	client, err := kube.NewInClusterClient(getenv)
	if err != nil {
		return abort(stdout, string(initial.Mode), err)
	}
	runner := reconcile.Runner{
		ReadDesired: func() (reconcile.DesiredSet, error) {
			again, err := readFiles()
			if err != nil {
				return reconcile.DesiredSet{}, err
			}
			return reconcile.ParseDesired(again)
		},
		Now:    time.Now,
		Client: client,
		Out:    stdout,
	}
	snap, err := reconcile.Read(initial, client)
	if err != nil {
		return abort(stdout, string(initial.Mode), err)
	}
	if err := runner.Run(reconcile.Classify(initial, snap, time.Now())); err != nil {
		return abort(stdout, string(initial.Mode), err)
	}
	return 0
}
