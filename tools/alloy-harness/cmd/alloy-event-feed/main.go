// Command alloy-event-feed runs the rendered agent-events pipeline in Alloy.
// Usage: alloy-event-feed <alloy> <host.alloy> <feed-dir> <ledger-dir>
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"github.com/RigelBuild/nebula/tools/alloy-harness/internal/check"
	"github.com/RigelBuild/nebula/tools/alloy-harness/internal/eventfeed"
)

const usage = "usage: alloy-event-feed <alloy> <host.alloy> <feed-dir> <ledger-dir>"

func main() {
	// Process root: the one place a context is created, so signals reach the run.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := mainWith(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// mainWith runs the feed harness and returns the exit code. The ledger dir is kept
// in argv only for the sentinel fixture; it is never read.
func mainWith(ctx context.Context, args []string, stdout, stderr *os.File) int {
	if len(args) != 4 || slices.Contains(args, "") {
		return check.Usage(stderr, usage)
	}
	checks, err := eventfeed.Run(ctx, args[0], args[1], args[2])
	if err != nil {
		slog.Error("alloy-event-feed failed", "err", err)
		return 1
	}
	return check.Report(stdout, stderr, checks)
}
