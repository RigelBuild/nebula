package main

import (
	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/retrieve"
	"github.com/spf13/cobra"
)

func newGetCmd() *cobra.Command {
	var options retrieve.Options
	command := &cobra.Command{
		Use:           "get",
		Short:         "Restore one archived transcript record",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return retrieve.Get(cmd.Context(), options)
		},
	}
	flags := command.Flags()
	flags.StringVar(&options.Repo, "repo", "", "restic repository path or URL")
	flags.StringVar(&options.Escrow, "escrow", "", "age-encrypted escrow file")
	flags.StringVar(&options.Identity, "identity", "", "age identity file")
	flags.StringVar(&options.Snapshot, "snapshot", "", "restic snapshot id")
	flags.StringVar(&options.Record, "record", "", "OMP record stem")
	flags.StringVar(&options.Output, "out", "", "output directory")
	if err := command.MarkFlagRequired("repo"); err != nil {
		return command
	}
	if err := command.MarkFlagRequired("escrow"); err != nil {
		return command
	}
	if err := command.MarkFlagRequired("identity"); err != nil {
		return command
	}
	if err := command.MarkFlagRequired("snapshot"); err != nil {
		return command
	}
	if err := command.MarkFlagRequired("record"); err != nil {
		return command
	}
	if err := command.MarkFlagRequired("out"); err != nil {
		return command
	}
	return command
}
