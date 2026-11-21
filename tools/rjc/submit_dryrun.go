package main

import (
	"context"
	"fmt"
	"io"
)

// submitDryRun reports what a real submit would do without moving any local
// bookmark or rewriting any commit. It runs after the shared preflight. It writes
// each candidate for review, then reports. The rebase, engine, and push checks
// run only on a real submit.
func (r *repository) submitDryRun(target, adoptionTip string, prepared []preparedChange, log io.Writer) error {
	store := fileBaseline{root: r.stateDir, keys: r.keys}
	for i := range prepared {
		var err error
		prepared[i].candidate.path, err = store.candidate(prepared[i].change.Bookmark, prepared[i].candidate.body)
		if err != nil {
			return err
		}
	}
	if adoptionTip != "" {
		_, err := fmt.Fprintf(log, "%s: would adopt origin at %s; rebase and engine checks run only on real submit\n", target, adoptionTip)
		return err
	}
	if target == "" {
		name := prepared[0].change.Bookmark
		r.submittedBookmark = name
		_, err := fmt.Fprintf(log, "%s: would create bookmark; rebase and engine checks run only on real submit\n", name)
		return err
	}
	r.submittedBookmark = target
	return nil
}

// preflightSubmit runs the shared read-only checks for a real or dry submit.
// A dry run with --at checks the planned retarget line as is, because it moves
// no bookmark.
func (r *repository) preflightSubmit(ctx context.Context, target string, opt submitOptions, plan retargetPlan, adoptionTip string, issues []int, title, body string, reviewPlan *reviewFixPlan, top string) ([]preparedChange, error) {
	if opt.dryRun && plan.bookmark != "" {
		return r.preflightLine(ctx, target, plan.line, opt, nil)
	}
	if target == "" {
		return r.preflightAdoption(ctx, opt, issues, title, body)
	}
	selectionTip := adoptionTip
	if plan.bookmark != "" {
		selectionTip = ""
	}
	line, err := r.submitPreflightLine(ctx, target, top, selectionTip, opt, reviewPlan)
	if err != nil {
		return nil, err
	}
	return r.preflightLine(ctx, target, line, opt, reviewPlan)
}

// preflightLine runs the read-only checks on a line whose tips are known.
func (r *repository) preflightLine(ctx context.Context, target string, line []Change, opt submitOptions, reviewPlan *reviewFixPlan) ([]preparedChange, error) {
	if err := r.refuseEmptyChange(ctx, line); err != nil {
		return nil, err
	}
	prepared, err := r.prepareLine(ctx, line, target, opt)
	if err != nil {
		return nil, err
	}
	prepared = publishedSubmitLine(prepared, target, reviewPlan)
	setReviewFixTip(prepared, reviewPlan)
	if err := r.preparePayloads(ctx, prepared, target, opt); err != nil {
		return nil, err
	}
	return prepared, nil
}

func (r *repository) preflightAdoption(ctx context.Context, opt submitOptions, issues []int, title, body string) ([]preparedChange, error) {
	prospective, err := r.keys.BookmarkName(r.codename, issues[0], title)
	if err != nil {
		return nil, err
	}
	if _, _, err := r.adoptionPlan(ctx, "@-", r.profile.TrunkRemote()); err != nil {
		return nil, err
	}
	existing, err := r.revisions(ctx, "bookmarks(exact:"+quoteRev(prospective)+")")
	if err != nil {
		return nil, err
	}
	if len(existing) != 0 {
		return nil, refusal("new", "bookmark-exists", prospective+" already exists", "use rjc submit "+prospective+" for existing work")
	}
	tip, err := r.one(ctx, "@-")
	if err != nil {
		return nil, err
	}
	change := Change{Bookmark: prospective, Tip: tip, Base: r.profile.TrunkRemote(), Issue: issues[0], Owned: true}
	c := candidate{title: title, body: body}
	if err := r.checkCandidateWithInputs(ctx, change, c, opt.skip, opt.template, issues); err != nil {
		return nil, err
	}
	if err := r.checkForeignPublished(ctx, []Change{change}, map[string]bool{change.Bookmark: true}); err != nil {
		return nil, err
	}
	prepared, err := r.prepareLine(ctx, []Change{change}, prospective, opt)
	if err != nil {
		return nil, err
	}
	prepared[0].candidate = c
	return prepared, nil
}
