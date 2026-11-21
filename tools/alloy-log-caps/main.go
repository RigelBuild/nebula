// Command alloy-log-caps runs captured log lines through the shipped Alloy stages.
// Usage: alloy-log-caps <pod-alloy> <host-alloy> <k8s-monitoring.yaml> <host-config.alloy> <aks-k8s-monitoring.yaml>
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"github.com/RigelBuild/nebula/tools/alloy-log-caps/internal/scenario"
)

const usage = "usage: alloy-log-caps <pod-alloy> <host-alloy> <k8s-monitoring.yaml> <host-config.alloy> <aks-k8s-monitoring.yaml>"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := mainWith(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// mainWith parses argv, runs every scenario, and returns the process exit code.
func mainWith(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) != 5 || slices.Contains(args, "") {
		scenario.WriteLine(stderr, usage)
		return 1
	}
	h := &scenario.Harness{
		Out:        stdout,
		PodAlloy:   args[0],
		HostAlloy:  args[1],
		PodYAML:    args[2],
		HostConfig: args[3],
		AKSYAML:    args[4],
	}
	if err := h.Run(ctx); err != nil {
		slog.Error("alloy-log-caps failed", "err", err)
		return 1
	}
	if n := h.Failures(); n > 0 {
		scenario.WriteLine(stderr, fmt.Sprintf("%d log-cap check(s) failed", n))
		return 1
	}
	return 0
}
