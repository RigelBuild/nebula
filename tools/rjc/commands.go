package main

import (
	"context"
	"errors"

	"github.com/spf13/cobra"
)

func requiredFlags(cmd *cobra.Command, names ...string) error {
	for _, name := range names {
		if err := cmd.MarkFlagRequired(name); err != nil {
			return err
		}
	}
	return nil
}

func (c *cli) root() (*cobra.Command, error) {
	root := &cobra.Command{Use: "rjc", Short: "Create and submit owned jj changes with checked PR metadata.", SilenceUsage: true, SilenceErrors: true}
	root.SetOut(c.stdout)
	root.SetErr(c.stderr)
	root.PersistentFlags().StringVarP(&c.output, "output", "o", outputText, "Output format: text or json.")
	root.PersistentFlags().StringVar(&c.codename, "codename", "", "Caller codename (otherwise RJC_CODENAME or workspace path).")
	root.PersistentFlags().StringVar(&c.profilesFlag, "profiles", "", "Profile CUE file naming the repositories and deployment values (otherwise RJC_PROFILES; no default).")
	for _, builder := range []func() (*cobra.Command, error){c.newCommand, c.submitCommand, c.metaCommand, c.syncCommand, c.upstreamSyncCommand, c.noteCommand, c.readyCommand, c.statusCommand, c.retireCommand} {
		cmd, err := builder()
		if err != nil {
			return nil, err
		}
		root.AddCommand(cmd)
	}
	return root, nil
}

func (c *cli) syncCommand() (*cobra.Command, error) {
	cmd := &cobra.Command{Use: "sync [bookmark]", Short: "Restack an owned line without pushing.", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ctx, r, err := c.initialize(cmd.Context())
		if err != nil {
			return toolFailure("preflight", err)
		}
		if err := prepareForge(ctx, r); err != nil {
			return toolFailure("preflight", err)
		}
		opt := syncOptions{}
		if len(args) > 0 {
			opt.bookmark = args[0]
		}
		results, err := r.sync(ctx, opt, c.logfile)
		if err != nil {
			return toolFailure("sync", err)
		}
		for _, result := range results {
			c.message += result.Bookmark + " " + result.Tip + "\n"
		}
		c.message += "rerun affected checks, then rjc submit"
		return nil
	}}
	return cmd, nil
}

func (c *cli) newCommand() (*cobra.Command, error) {
	opt := newOptions{}
	cmd := &cobra.Command{Use: "new", Short: "Create an owned bookmark and disarm its working copy.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, r, err := c.initialize(cmd.Context())
		if err != nil {
			return toolFailure("preflight", err)
		}
		if err := r.keys.validateAuthoredTitle(opt.title); err != nil {
			return err
		}
		if err := r.fetchMain(ctx); err != nil {
			return err
		}
		bookmark, err := r.newChange(ctx, opt)
		if err != nil {
			return toolFailure("new", err)
		}
		c.message = "Created " + bookmark + "; working copy is disarmed."
		return nil
	}}
	cmd.Flags().StringArrayVar(&opt.issues, "issue", nil, "Issue <PREFIX>-<n>; repeat for multiple issues (first is primary).")
	cmd.Flags().StringVar(&opt.title, "title", "", "One-line Conventional Commits title.")
	cmd.Flags().StringVar(&opt.on, "on", "", "Exact parent bookmark (owned unpublished local or parent@origin).")
	cmd.Flags().StringVar(&opt.at, "at", "", "Existing committed descendant to bookmark.")
	return cmd, requiredFlags(cmd, "issue", "title")
}

func (c *cli) submitCommand() (*cobra.Command, error) {
	opt := submitOptions{}
	var meta metadataFlags
	pairs := &skipPairs{targets: &opt.skippedTargets}
	cmd := &cobra.Command{Use: "submit [bookmark]", Short: "Check, push through jj-vine, and reconcile exact PR metadata.", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if pairs.pending {
			return &failure{stage: usageStage, gate: "skip-target", reason: "each --skip-target needs a following --reason", code: 2}
		}
		var err error
		opt.title, err = meta.composeTitle(opt.title)
		if err != nil {
			return err
		}
		opt.sections, err = meta.resolve()
		if err != nil {
			return err
		}
		if len(args) > 0 {
			opt.bookmark = args[0]
		}
		opt.skipGiven = cmd.Flags().Changed("skip-checks")
		opt.holdGiven = cmd.Flags().Changed("hold")
		if err := validateSubmitOptions(opt); err != nil {
			return err
		}
		ctx, r, err := c.initialize(cmd.Context())
		if err != nil {
			return toolFailure("preflight", err)
		}
		if err := r.keys.validateAuthoredTitle(opt.title); err != nil {
			return err
		}
		if err := prepareForge(ctx, r); err != nil {
			return toolFailure("preflight", err)
		}
		s, err := service(ctx)
		if err != nil {
			return toolFailure("preflight", err)
		}
		var engine JJVine = vine{repo: r}
		if s.engine != nil {
			engine = s.engine
		}
		if err := r.submit(ctx, opt, engine, c.logfile); err != nil {
			return toolFailure(submitCommandName, err)
		}
		opt.bookmark = r.submittedBookmark
		c.message = r.describeSubmit(opt)
		return nil
	}}
	bindMetadataInput(cmd, &opt.title, &opt.bodyFile)
	cmd.Flags().StringArrayVar(&opt.issues, "issue", nil, "Issue <PREFIX>-<n> for unbookmarked work; repeat (first is primary).")
	cmd.Flags().StringVar(&opt.ack, "ack-remote", "", "Exact sha256 of the freshly read remote authored body.")
	cmd.Flags().StringVar(&opt.skip, "skip-checks", "", "Bypass reason: one line up to 200 characters, or main-red:<reference>, or p0:<PREFIX>-<n>.")
	cmd.Flags().StringVar(&opt.hold, "hold", "", "Post a Hold: <reason> PR comment before submit returns, so nemo-merge sees it at once.")
	cmd.Flags().Var(&skipFlag{pairs: pairs}, "skip-target", "Leave out one failed project:task; follow with --reason.")
	cmd.Flags().Var(&skipFlag{pairs: pairs, reason: true}, "reason", "Reason for the preceding --skip-target.")
	cmd.Flags().StringVar(&opt.at, "at", "", "Retarget the selected bookmark to this existing commit.")
	cmd.Flags().BoolVar(&opt.retarget, "retarget", false, "Retarget the PR base to trunk.")
	cmd.Flags().BoolVar(&opt.dryRun, "dry-run", false, "Prepare and validate locally without pushing or forge writes.")
	cmd.Flags().BoolVar(&opt.review, "review", false, "Queue an agent review round after push and reconciliation.")
	cmd.Flags().BoolVar(&opt.draft, "draft", false, "Open a real work-in-progress PR as draft; add an Updating: or Hold: PR comment.")
	meta.bind(cmd)
	return cmd, requiredFlags(cmd, "title")
}

func prepareForge(ctx context.Context, r *repository) error {
	s, err := service(ctx)
	if err != nil {
		return err
	}
	if s.forge != nil {
		return nil
	}
	s.forge, err = r.forge(ctx)
	return err
}

func (c *cli) metaCommand() (*cobra.Command, error) {
	var title, bodyFile string
	var meta metadataFlags
	cmd := &cobra.Command{Use: "meta <bookmark>", Short: "Save checked title and body metadata for an owned bookmark.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		var err error
		title, err = meta.composeTitle(title)
		if err != nil {
			return err
		}
		sections, err := meta.resolve()
		if err != nil {
			return err
		}
		ctx, r, err := c.initialize(cmd.Context())
		if err != nil {
			return toolFailure("preflight", err)
		}
		if err := r.keys.validateAuthoredTitle(title); err != nil {
			return err
		}
		if err := r.fetchMain(ctx); err != nil {
			return err
		}
		if err := r.saveCandidate(ctx, args[0], title, bodyFile, sections); err != nil {
			return toolFailure("metadata", err)
		}
		c.message = "Saved metadata for " + args[0]
		return nil
	}}
	bindMetadataInput(cmd, &title, &bodyFile)
	meta.bind(cmd)
	return cmd, requiredFlags(cmd, "title")
}

func bindMetadataInput(cmd *cobra.Command, title, bodyFile *string) {
	cmd.Flags().StringVar(title, "title", "", "PR title or subject when using --type.")
	cmd.Flags().StringVar(bodyFile, "body-file", "", "Optional authored body file; defaults to the PR template.")
}

type skipPairs struct {
	targets *[]SkippedTarget
	pending bool
}
type skipFlag struct {
	pairs  *skipPairs
	reason bool
}

func (f *skipFlag) String() string { return "" }
func (f *skipFlag) Type() string   { return "string" }
func (f *skipFlag) Set(value string) error {
	if f.reason {
		if !f.pairs.pending {
			return errors.New("--reason must follow --skip-target")
		}
		targets := *f.pairs.targets
		targets[len(targets)-1].Reason = value
		f.pairs.pending = false
		return nil
	}
	if f.pairs.pending {
		return errors.New("--skip-target requires a following --reason")
	}
	*f.pairs.targets = append(*f.pairs.targets, SkippedTarget{Target: value})
	f.pairs.pending = true
	return nil
}
