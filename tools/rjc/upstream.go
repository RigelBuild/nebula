package main

import (
	"context"
	"io"

	"github.com/spf13/cobra"
)

type upstreamSyncOptions struct{ issue, upstream string }

func (c *cli) upstreamSyncCommand() (*cobra.Command, error) {
	opt := upstreamSyncOptions{}
	cmd := &cobra.Command{Use: "upstream-sync", Short: "Create an upstream merge for a tracking fork.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, r, err := c.initialize(cmd.Context())
		if err != nil {
			return toolFailure("preflight", err)
		}
		bookmark, err := r.upstreamSync(ctx, opt, c.stdout)
		if err != nil {
			return toolFailure("upstream-sync", err)
		}
		if bookmark == "" {
			c.message = "already-synced"
		} else {
			c.message = "Created " + bookmark + "; working copy is disarmed."
		}
		return nil
	}}
	cmd.Flags().StringVar(&opt.issue, "issue", "", "Issue <PREFIX>-<n>.")
	cmd.Flags().StringVar(&opt.upstream, "upstream", "", "Upstream revision (default mirror at origin).")
	return cmd, requiredFlags(cmd, "issue")
}

func (r *repository) upstreamSync(ctx context.Context, opt upstreamSyncOptions, log io.Writer) (string, error) {
	if r.profile.Posture != forkTracking {
		return "", refusal("upstream-sync", "not-tracking-fork", "repository is not a tracking fork", "use upstream-sync on a tracking fork")
	}
	issue, err := r.keys.issueNumber(opt.issue)
	if err != nil {
		return "", err
	}
	if err := r.clean(ctx); err != nil {
		return "", err
	}
	if err := r.fetchMain(ctx); err != nil {
		return "", err
	}
	release, err := r.one(ctx, quoteRev(r.profile.TrunkRemote()))
	if err != nil {
		return "", err
	}
	upstream := opt.upstream
	if upstream == "" {
		upstream = r.profile.Mirror + "@origin"
	}
	u, err := r.one(ctx, quoteRev(upstream))
	if err != nil {
		return "", err
	}
	mirror := quoteRev(r.profile.Mirror + "@origin")
	inMirror, err := r.revisions(ctx, quoteRev(u)+" & ::"+mirror)
	if err != nil {
		return "", err
	}
	if len(inMirror) != 1 {
		return "", refusal("upstream-sync", "outside-mirror", "upstream revision is not in the mirror", "select a revision in the mirror")
	}
	contained, err := r.revisions(ctx, quoteRev(u)+" & ::"+quoteRev(release))
	if err != nil {
		return "", err
	}
	if len(contained) != 0 {
		return "", nil
	}
	title, err := r.keys.FullTitle("chore(fork): merge upstream main "+u[:12], []int{issue})
	if err != nil {
		return "", err
	}
	bookmark, err := r.keys.BookmarkName(r.codename, issue, title)
	if err != nil {
		return "", err
	}
	existing, err := r.revisions(ctx, "bookmarks(exact:"+quoteRev(bookmark)+")")
	if err != nil {
		return "", err
	}
	if len(existing) != 0 {
		return "", refusal("upstream-sync", "bookmark-exists", bookmark+" already exists", "submit the existing bookmark")
	}
	if _, err := r.jj(ctx, "new", release, u, "-m", title+"\n\nCo-authored-by: "+r.profile.CoAuthor); err != nil {
		return "", err
	}
	change, err := r.jj(ctx, "log", "--no-graph", "-r", "@", "-T", "change_id")
	if err != nil {
		return "", err
	}
	if _, err := r.jj(ctx, "bookmark", "create", bookmark, "-r", "@"); err != nil {
		return "", err
	}
	if err := (fileBaseline{root: r.stateDir, keys: r.keys}).SaveUpstream(bookmark, UpstreamRecord{Repo: r.profile.Name, Release: release, Upstream: u, Change: change}); err != nil {
		return "", err
	}
	if err := r.saveIssues(bookmark, []int{issue}); err != nil {
		return "", err
	}
	if _, err := r.jj(ctx, "new", quoteRev(bookmark)); err != nil {
		return "", err
	}
	conflicts, err := r.revisions(ctx, "conflicts() & "+quoteRev(bookmark))
	if err != nil {
		return "", err
	}
	if len(conflicts) != 0 {
		files, err := r.jj(ctx, "resolve", "--list", "-r", quoteRev(bookmark))
		if err != nil {
			return "", err
		}
		return "", refusal("upstream-sync", "conflict", "merge has conflicts:\n"+files, "resolve in the child and jj squash into the merge")
	}
	_ = log
	return bookmark, nil
}
