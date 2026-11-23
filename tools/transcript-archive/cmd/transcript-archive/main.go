package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/fence"
	"github.com/spf13/cobra"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newRootCmd().ExecuteContext(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitCode(err))
	}
}

func newRootCmd() *cobra.Command {
	command := &cobra.Command{
		Use:           "transcript-archive",
		Short:         "Inventory and verify transcript archives",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	command.AddCommand(newInventoryCmd(), newCoverageCmd(), newGetCmd(), newFenceCmd(), newPassCmd(), newRawCapCmd())
	return command
}

func exitCode(err error) int {
	if exitErr, ok := errors.AsType[*fence.ExitError](err); ok {
		return exitErr.Code
	}
	return 1
}
