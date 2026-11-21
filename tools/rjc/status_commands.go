package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func (c *cli) statusCommand() (*cobra.Command, error) {
	var opt statusOptions
	cmd := &cobra.Command{Use: "status", Short: "Summarize owned PRs.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, r, err := c.initialize(cmd.Context())
		if err != nil {
			return toolFailure("preflight", err)
		}
		if err := prepareForge(ctx, r); err != nil {
			return toolFailure("preflight", err)
		}
		rows, err := r.status(ctx, opt)
		if err != nil {
			return toolFailure("status", err)
		}
		c.statusRows = rows
		var b strings.Builder
		b.WriteString("PR\tSTATE\tCONFLICTS\tHOOKS\tREVIEW\tAPPROVAL\tCI\tNEXT\n")
		for _, row := range rows {
			fmt.Fprintf(&b, "%s#%d (%s)\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", row.Repo, row.PR, row.Bookmark, row.State, row.Conflicts, row.Hooks, row.Review, row.Approval, row.CI, row.Next)
		}
		c.message = strings.TrimSpace(b.String())
		return nil
	}}
	cmd.Flags().StringVar(&opt.repo, "repo", "", "Limit to owner/repo.")
	cmd.Flags().BoolVar(&opt.all, "all", false, "Include merged and closed PRs.")
	return cmd, nil
}

func (c *cli) retireCommand() (*cobra.Command, error) {
	var opt retireOptions
	cmd := &cobra.Command{Use: "retire <bookmark>", Short: "Delete an owned closed PR bookmark.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ctx, r, err := c.initialize(cmd.Context())
		if err != nil {
			return toolFailure("preflight", err)
		}
		if err := prepareForge(ctx, r); err != nil {
			return toolFailure("preflight", err)
		}
		opt.bookmark = args[0]
		if err := r.retire(ctx, opt, c.logfile); err != nil {
			return toolFailure("retire", err)
		}
		c.message = "Retired " + opt.bookmark
		return nil
	}}
	cmd.Flags().BoolVar(&opt.noPR, "no-pr", false, "Retire a bookmark that never had a PR.")
	return cmd, nil
}
