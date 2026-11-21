package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

func syncPublishedAncestors(plan syncPlan) string {
	bookmarks := make([]string, 0, len(plan.Segments))
	for _, segment := range plan.Segments {
		bookmarks = append(bookmarks, "remote_bookmarks(exact:"+quoteRev(segment.Bookmark)+", remote=exact:\"origin\")")
	}
	return "::(" + strings.Join(bookmarks, " | ") + ")"
}

// checkForeignBoundary refuses before any rewrite when an unowned parent would
// become the rebase base above an owned local bookmark. line() refuses that same
// stack after the rebase, so this only moves the refusal ahead of the rewrite.
func (r *repository) checkForeignBoundary(ctx context.Context, plan syncPlan) error {
	foreign, ok := strings.CutSuffix(plan.Destination, "@origin")
	if !ok || foreign == r.profile.Trunk || r.keys.Owned(r.codename, foreign) {
		return nil
	}
	below, err := r.localBookmarks(ctx, "("+quoteRev(r.profile.TrunkRemote())+".."+quoteRev(plan.Destination)+") ~ "+quoteRev(plan.Destination))
	if err != nil {
		return err
	}
	for _, bookmark := range below {
		if r.keys.Owned(r.codename, bookmark) {
			return refusal("ownership", "foreign-bookmark", "another caller's bookmark interrupts the line: "+foreign, "submit only a line based on "+foreign+"@origin")
		}
	}
	return nil
}

func (r *repository) checkSyncPlan(ctx context.Context, plan syncPlan) error {
	if err := r.checkForeignBoundary(ctx, plan); err != nil {
		return err
	}
	all := append([]string{}, plan.Stale...)
	roots := make([]string, 0, len(plan.Segments))
	for _, segment := range plan.Segments {
		all = append(all, segment.Commits...)
		roots = append(roots, segment.Root)
		tip, err := r.one(ctx, quoteRev(segment.Bookmark))
		if err != nil {
			return err
		}
		if !slices.Contains(segment.Commits, tip) {
			return refusal("ownership", "outside-line", "bookmark is outside its planned segment", "replan the selected line")
		}
	}
	roots = append(roots, plan.Stale...)
	bookmarks, err := r.stackBookmarks(ctx, "::remote_bookmarks(remote=exact:\"origin\") & descendants("+syncSet(roots)+")")
	if err != nil {
		return err
	}
	owned := map[string]bool{}
	for _, segment := range plan.Segments {
		owned[segment.Bookmark+"@origin"] = true
	}
	for _, bookmark := range bookmarks {
		if strings.HasSuffix(bookmark, "@origin") && !owned[bookmark] {
			return refusal("ownership", "divergent-line", "published bookmark outside the plan reaches rewritten commits: "+bookmark, "separate the published line before syncing")
		}
	}
	if len(plan.Stale) > 0 {
		outside, err := r.revisions(ctx, "descendants("+syncSet(plan.Stale)+") ~ "+syncSet(all)+" ~ (working_copies() & empty() & description(exact:\"\"))")
		if err != nil {
			return err
		}
		if len(outside) > 0 {
			return refusal("ownership", "divergent-line", "stale commits have descendants outside the sync plan: "+strings.Join(outside, ", "), "confirm these are old copies, then jj abandon "+strings.Join(outside, " "))
		}
		published, err := r.revisions(ctx, syncPublishedAncestors(plan)+" & descendants("+syncSet(plan.Stale)+") ~ "+syncSet(plan.Stale))
		if err != nil {
			return err
		}
		if len(published) > 0 {
			return refusal("ownership", "divergent-line", "published commits descend from stale commits: "+strings.Join(published, ", "), "separate the published line before syncing")
		}
	}
	top := plan.Segments[len(plan.Segments)-1].Bookmark
	allowedWC, err := r.revisions(ctx, "@ & empty() & description(exact:\"\") & children("+quoteRev(top)+")")
	if err != nil {
		return err
	}
	all = append(all, allowedWC...)
	outside, err := r.revisions(ctx, "descendants("+syncSet(roots)+") ~ "+syncSet(all))
	if err != nil {
		return err
	}
	if len(outside) != 0 {
		return refusal("ownership", "outside-line", "sync would rewrite commits outside its plan: "+strings.Join(outside, ", "), "separate the selected line")
	}
	bookmarks, err = r.stackBookmarks(ctx, syncSet(all))
	if err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, segment := range plan.Segments {
		allowed[segment.Bookmark] = true
	}
	for _, bookmark := range bookmarks {
		name := strings.TrimSuffix(bookmark, "@origin")
		if !allowed[name] {
			return refusal("ownership", "outside-line", "sync would rewrite a bookmark outside its plan: "+bookmark, "separate the selected line")
		}
	}
	merges, err := r.revisions(ctx, "merges() & "+syncSet(all))
	if err != nil {
		return err
	}
	if len(merges) != 0 {
		return refusal("ownership", "nonlinear", "sync line contains a merge", "select a linear owned line")
	}
	return nil
}

func (r *repository) syncRecovery(ctx context.Context, plan syncPlan, tipsBefore []string) (string, error) {
	if len(plan.Segments) != len(tipsBefore) {
		return "", errors.New("sync recovery segment count mismatch")
	}
	fixes := make([]string, 0, len(plan.Segments)+1)
	created := make([]string, 0)
	for i, tipBefore := range tipsBefore {
		segment := plan.Segments[i]
		tip, err := r.one(ctx, quoteRev(segment.Bookmark))
		if err != nil {
			return "", err
		}
		if tipBefore != "" && tip != tipBefore {
			fixes = append(fixes, fmt.Sprintf("jj bookmark set %s -r %s --allow-backwards", segment.Bookmark, tipBefore))
		}
		for _, old := range segment.Commits {
			id, err := r.jj(ctx, "log", "--no-graph", "-r", quoteRev(old), "-T", "change_id")
			if err != nil {
				return "", err
			}
			current, err := r.one(ctx, quoteRev(id))
			if err != nil {
				return "", err
			}
			if current != old {
				created = append(created, current)
			}
		}
	}
	if len(created) > 0 {
		fixes = append(fixes, "jj abandon "+strings.Join(created, " "))
	}
	return strings.Join(fixes, "\n"), nil
}

//nolint:cyclop // The ordered rebase and recovery steps form one transaction.
func (r *repository) sync(ctx context.Context, opt syncOptions, log io.Writer) ([]SyncResult, error) {
	if err := r.clean(ctx); err != nil {
		return nil, err
	}
	if err := r.fetchMain(ctx); err != nil {
		return nil, err
	}
	target := opt.bookmark
	if target == "" {
		bookmarks, err := r.localBookmarks(ctx, quoteRev(r.profile.TrunkRemote())+"..@-")
		if err != nil {
			return nil, err
		}
		for _, bookmark := range bookmarks {
			if r.keys.Owned(r.codename, bookmark) {
				target = bookmark
			}
		}
	}
	if target == "" || !r.keys.Owned(r.codename, target) {
		return nil, refusal("ownership", "foreign-bookmark", "no owned bookmark selected", "select an owned bookmark")
	}
	if err := r.refuseConflictedBookmark(ctx, target); err != nil {
		return nil, err
	}
	tip, err := r.one(ctx, quoteRev(target))
	if err != nil {
		return nil, err
	}
	rec, recorded, err := r.recordedUpstreamMerge(ctx, target, tip)
	if err != nil {
		return nil, err
	}
	if recorded {
		return r.syncUpstreamMerge(ctx, target, log, rec)
	}
	plan, err := r.planSync(ctx, target)
	if err != nil {
		return nil, err
	}
	if err := r.checkSyncPlan(ctx, plan); err != nil {
		return nil, err
	}
	ids := make([]string, len(plan.Segments))
	tipsBefore := make([]string, len(plan.Segments))
	for i, segment := range plan.Segments {
		ids[i], err = r.jj(ctx, "log", "--no-graph", "-r", quoteRev(segment.Root), "-T", "change_id")
		if err != nil {
			return nil, err
		}
		tipsBefore[i], err = r.one(ctx, quoteRev(segment.Bookmark))
		if err != nil {
			return nil, err
		}
	}
	moved := 0
	fail := func(err error) error {
		if moved == 0 {
			return err
		}
		fix, recoveryErr := r.syncRecovery(ctx, plan, tipsBefore)
		if recoveryErr != nil {
			return errors.Join(err, recoveryErr)
		}
		if f, ok := errors.AsType[*failure](err); ok {
			failureCopy := *f
			failureCopy.fix += "\n" + fix
			return &failureCopy
		}
		return &failure{stage: "sync", gate: toolFailureGate, reason: err.Error(), fix: fix, code: 3, cause: err}
	}
	for i := range plan.Segments {
		dest := plan.Destination
		if i > 0 {
			dest = plan.Segments[i-1].Bookmark
		}
		root, err := r.one(ctx, quoteRev(ids[i]))
		if err != nil {
			return nil, fail(err)
		}
		parents, err := r.revisions(ctx, quoteRev(root)+"- & "+quoteRev(dest))
		if err != nil {
			return nil, fail(err)
		}
		if len(parents) == 0 {
			// Owned published commits are immutable under the jj configuration.
			if _, err := r.jj(ctx, "rebase", "--ignore-immutable", "-s", root, "-d", quoteRev(dest)); err != nil {
				return nil, fail(err)
			}
			moved++
		}
	}
	results := make([]SyncResult, 0, len(plan.Segments))
	for _, segment := range plan.Segments {
		tip, err := r.one(ctx, quoteRev(segment.Bookmark))
		if err != nil {
			return nil, fail(err)
		}
		base := plan.Destination
		if segment.Parent != "" {
			base = segment.Parent
		}
		conflicts, err := r.revisions(ctx, "conflicts() & ("+quoteRev(base)+".."+quoteRev(tip)+")")
		if err != nil {
			return nil, fail(err)
		}
		if len(conflicts) > 0 {
			fix := make([]string, len(conflicts))
			for i, id := range conflicts {
				fix[i] = "jj new " + id
			}
			return nil, fail(refusal("rebase", "conflict", "conflicted commits: "+strings.Join(conflicts, ", "), strings.Join(fix, "; ")))
		}
		results = append(results, SyncResult{Bookmark: segment.Bookmark, Base: base, Tip: tip})
	}
	if _, err := r.line(ctx, target, lineOptions{}); err != nil {
		return nil, fail(err)
	}
	if err := r.reparentSaved(ctx, plan.Reparent); err != nil {
		return nil, fail(err)
	}
	for _, result := range results {
		if _, err := fmt.Fprintf(log, "%s %s\n", result.Bookmark, result.Tip); err != nil {
			return nil, fail(err)
		}
	}
	if len(plan.Stale) > 0 {
		outside, err := r.revisions(ctx, "descendants("+syncSet(plan.Stale)+") ~ "+syncSet(plan.Stale)+" ~ "+syncPublishedAncestors(plan))
		if err != nil {
			return nil, fail(err)
		}
		if len(outside) != 0 {
			return nil, fail(refusal("ownership", "outside-line", "stale parent still has descendants", "separate the selected line"))
		}
		if _, err := r.jj(ctx, "abandon", syncSet(plan.Stale)); err != nil {
			return nil, fail(err)
		}
	}
	if plan.Stop != nil {
		plan.Stop.Landed = true
		if err := r.saveParent(ctx, plan.StopBookmark, *plan.Stop); err != nil {
			return nil, fail(err)
		}
	}
	return results, nil
}
