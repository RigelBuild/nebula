package main

import (
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/fence"
	"github.com/spf13/cobra"
)

func newFenceCmd() *cobra.Command {
	var agentDirs []string
	var wait time.Duration
	command := &cobra.Command{
		Use:           "fence",
		Short:         "Hold OMP garbage collection locks",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			return fence.Run(ctx, fence.Options{AgentDirs: agentDirs, Wait: wait})
		},
	}
	command.Flags().StringArrayVar(&agentDirs, "agent-dir", nil, "agent directory to fence (repeat for multiple directories)")
	command.Flags().DurationVar(&wait, "wait", 30*time.Minute, "maximum time to wait for an existing lock")
	return command
}
