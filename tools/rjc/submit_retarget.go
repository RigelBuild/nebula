package main

import (
	"context"
	"fmt"
	"io"

	"github.com/samber/oops"
)

type retargetPlan struct {
	bookmark, revision, oldTip, newTip string
	line                               []Change
}

func (r *repository) prepareRetarget(ctx context.Context, bookmark, revision string) (retargetPlan, error) {
	if revision == "" {
		return retargetPlan{}, nil
	}
	plan, err := r.preflightBookmarkRetarget(ctx, bookmark, revision)
	if err != nil {
		return retargetPlan{}, err
	}
	return *plan, nil
}

func (r *repository) moveOptionalRetarget(ctx context.Context, plan retargetPlan, log io.Writer) error {
	if plan.bookmark == "" {
		return nil
	}
	return r.moveRetargetBookmark(ctx, plan, log)
}

func (r *repository) restoreRetarget(ctx context.Context, plan retargetPlan, submitErr error) error {
	if plan.bookmark == "" {
		return submitErr
	}
	if _, err := r.jj(ctx, "bookmark", "set", plan.bookmark, "-r", plan.oldTip, "--allow-backwards"); err != nil {
		if submitErr != nil {
			return fmt.Errorf("%w; restoring bookmark: %w", submitErr, err)
		}
		return fmt.Errorf("restoring bookmark: %w", err)
	}
	return submitErr
}

//nolint:cyclop // Preflight walks one ordered ownership and ancestry chain; each refusal depends on the state the step before it read.
func (r *repository) preflightBookmarkRetarget(ctx context.Context, bookmark, revision string) (*retargetPlan, error) {
	if bookmark == "" || !r.keys.Owned(r.codename, bookmark) {
		return nil, refusal("retarget", "retarget-not-owned", "bookmark is not owned by the caller", "select an existing owned bookmark with an open PR")
	}
	local, err := r.revisions(ctx, "bookmarks(exact:"+quoteRev(bookmark)+")")
	if err != nil {
		return nil, err
	}
	if len(local) == 0 {
		return nil, refusal("retarget", "retarget-not-owned", "bookmark does not exist locally", "select an existing owned bookmark with an open PR")
	}
	if len(local) != 1 {
		return nil, refusal("retarget", "retarget-not-owned", "bookmark is conflicted locally", "resolve the bookmark conflict before submitting")
	}
	s, err := service(ctx)
	if err != nil {
		return nil, err
	}
	pr, err := s.forge.findPR(ctx, r.profile.Name, bookmark)
	if err != nil {
		return nil, err
	}
	if pr == 0 {
		return nil, refusal("retarget", "retarget-not-owned", "bookmark has no open PR", "select an existing owned bookmark with an open PR")
	}
	newTip, err := r.one(ctx, quoteRev(revision))
	if err != nil {
		return nil, err
	}
	base, parentBookmark, err := r.retargetBase(ctx, bookmark)
	if err != nil {
		return nil, err
	}
	if err := r.checkRetargetRange(ctx, bookmark, parentBookmark, base, newTip); err != nil {
		return nil, err
	}
	issue, err := r.keys.bookmarkIssue(bookmark)
	if err != nil {
		return nil, err
	}
	line := []Change{}
	if parentBookmark != "" && base == parentBookmark {
		line, err = r.line(ctx, parentBookmark, lineOptions{mode: linePreflight, skipOwnership: true})
		if err != nil {
			return nil, err
		}
	}
	if err := r.refreshParentBoundary(ctx, bookmark, newTip, linePreflight); err != nil {
		return nil, err
	}
	bookmarks, err := r.localBookmarks(ctx, quoteRev(base)+".."+quoteRev(newTip))
	if err != nil {
		return nil, err
	}
	for _, name := range bookmarks {
		if name == bookmark || name == parentBookmark || !r.keys.Owned(r.codename, name) {
			continue
		}
		intermediateIssue, err := r.keys.bookmarkIssue(name)
		if err != nil {
			return nil, err
		}
		tip, err := r.one(ctx, quoteRev(name))
		if err != nil {
			return nil, err
		}
		if err := r.refreshParentBoundary(ctx, name, tip, linePreflight); err != nil {
			return nil, err
		}
		if len(line) > 0 && line[len(line)-1].Tip == tip {
			return nil, oops.Errorf("multiple bookmarks point at one revision; select an unambiguous line")
		}
		line = append(line, Change{Bookmark: name, Tip: tip, Base: base, Owned: true, Issue: intermediateIssue})
		base = name
	}
	if len(line) > 0 && line[len(line)-1].Tip == newTip {
		return nil, oops.Errorf("multiple bookmarks point at one revision; select an unambiguous line")
	}
	line = append(line, Change{Bookmark: bookmark, Tip: newTip, Base: base, Owned: true, Issue: issue})
	if err := r.checkLandedBases(line); err != nil {
		return nil, err
	}
	if err := r.checkLinearOwnership(ctx, line, true); err != nil {
		return nil, err
	}
	if err := r.refuseDivergentLine(ctx, line); err != nil {
		return nil, err
	}
	return &retargetPlan{bookmark: bookmark, revision: revision, oldTip: local[0], newTip: newTip, line: line}, nil
}

func (r *repository) retargetBase(ctx context.Context, bookmark string) (string, string, error) {
	parent, err := r.syncParent(ctx, bookmark)
	if err != nil {
		return "", "", err
	}
	if parent == nil {
		return r.profile.TrunkRemote(), "", nil
	}
	base := parent.Bookmark + "@origin"
	if r.keys.Owned(r.codename, parent.Bookmark) {
		parentTip, err := r.revisions(ctx, "bookmarks(exact:"+quoteRev(parent.Bookmark)+")")
		if err != nil {
			return "", "", err
		}
		if len(parentTip) == 1 {
			base = parent.Bookmark
		}
	}
	if base == parent.Bookmark+"@origin" {
		remote, err := r.revisions(ctx, "remote_bookmarks(exact:"+quoteRev(parent.Bookmark)+", exact:\"origin\")")
		if err != nil {
			return "", "", err
		}
		if len(remote) != 1 {
			return "", "", refusal("retarget", "retarget-wrong-base", "recorded parent is missing from origin", "run rjc sync before retargeting")
		}
	}
	return base, parent.Bookmark, nil
}

func (r *repository) checkRetargetRange(ctx context.Context, bookmark, parentBookmark, base, newTip string) error {
	ancestors, err := r.revisions(ctx, quoteRev(base)+" & ::"+quoteRev(newTip))
	if err != nil {
		return err
	}
	if len(ancestors) != 1 {
		return refusal("retarget", "retarget-wrong-base", "selected commit is not above the bookmark base", "select a commit above the recorded parent or configured trunk")
	}
	rangeRev := quoteRev(base) + ".." + quoteRev(newTip)
	commits, err := r.revisions(ctx, rangeRev)
	if err != nil {
		return err
	}
	if len(commits) == 0 {
		return refusal("retarget", "retarget-wrong-base", "selected commit has no work above the bookmark base", "select a commit above the recorded parent or configured trunk")
	}
	merges, err := r.revisions(ctx, "merges() & ("+rangeRev+")")
	if err != nil {
		return err
	}
	if len(merges) != 0 {
		return refusal("retarget", "retarget-wrong-base", "selected range contains a merge", "select a linear commit range above the bookmark base")
	}
	bookmarks, err := r.localBookmarks(ctx, rangeRev)
	if err != nil {
		return err
	}
	for _, name := range bookmarks {
		if name == bookmark || name == parentBookmark || r.keys.Owned(r.codename, name) {
			continue
		}
		return refusal("retarget", "existing-bookmark", "selected range contains another local bookmark: "+name, "select a range without foreign bookmarks")
	}
	return nil
}

func (r *repository) moveRetargetBookmark(ctx context.Context, plan retargetPlan, log io.Writer) error {
	if _, err := r.jj(ctx, "bookmark", "set", plan.bookmark, "-r", plan.newTip, "--allow-backwards"); err != nil {
		return err
	}
	if log != nil {
		if _, err := fmt.Fprintf(log, "retarget bookmark %s: %s -> %s\n", plan.bookmark, plan.oldTip, plan.newTip); err != nil {
			return fmt.Errorf("log retargeted bookmark: %w", err)
		}
	}
	return nil
}
