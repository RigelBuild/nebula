package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

func (r *repository) rebaseOwned(ctx context.Context, line []Change, atRetarget bool, excused map[string]bool) error {
	sourceBase := line[0].Base
	changeID, err := r.jj(ctx, "log", "--no-graph", "-r", quoteRev(line[len(line)-1].Tip), "-T", "change_id")
	if err != nil {
		return err
	}
	plan, tipsBefore, err := r.lineSyncPlan(ctx, line)
	if err != nil {
		return err
	}
	allowed, root, stale, staleChanges, widened, err := r.checkRewrite(ctx, line, atRetarget, excused)
	if err != nil {
		return err
	}
	if err := r.abandonExcused(ctx, root, line, widened); err != nil {
		return r.rebaseFailure(ctx, plan, tipsBefore, fmt.Errorf("abandoning superseded heads: %w", err))
	}
	if _, err := r.jj(ctx, "rebase", "--ignore-immutable", "-s", root, "-d", quoteRev(sourceBase)); err != nil {
		return r.rebaseFailure(ctx, plan, tipsBefore, err)
	}
	if err := r.abandonStaleRewrites(ctx, stale, staleChanges, allowed); err != nil {
		return r.rebaseFailure(ctx, plan, tipsBefore, fmt.Errorf("cleaning stale rebase copies: %w", err))
	}
	tipRev := "bookmarks(exact:" + quoteRev(line[len(line)-1].Bookmark) + ")"
	if tips, err := r.revisions(ctx, tipRev); err != nil {
		return err
	} else if len(tips) == 0 {
		tipRev = "change_id(" + quoteRev(changeID) + ")"
	}
	conflictRev := "conflicts() & (" + quoteRev(sourceBase) + ".." + tipRev + ")"
	conflicts, err := r.revisions(ctx, conflictRev)
	if err != nil {
		return fmt.Errorf("checking conflicts: %w", err)
	}
	if len(conflicts) > 0 {
		summary, err := r.jj(ctx, "log", "-r", conflictRev)
		if err != nil {
			return fmt.Errorf("reading conflicts: %w", err)
		}
		var output strings.Builder
		output.WriteString(summary)
		for _, commit := range conflicts {
			files, err := r.jj(ctx, "resolve", "--list", "-r", commit)
			if err != nil {
				return fmt.Errorf("reading conflict files: %w", err)
			}
			output.WriteString("\n" + files)
		}
		return r.rebaseFailure(ctx, plan, tipsBefore, refusal("rebase", "conflict", output.String(), "resolve the conflicts before retrying"))
	}
	return nil
}

// abandonExcused abandons the excused own heads that a rebase of root would
// otherwise carry along. It never abandons a commit of the line being submitted
// (the range base..tip), and it keeps every position an owned bookmark of the
// line holds, locally or on origin: a recorded head can be a bookmark's live or
// origin tip, and abandoning that would move or delete the bookmark. An empty
// set does nothing, so callers that do not run the descendant check are
// unaffected.
func (r *repository) abandonExcused(ctx context.Context, root string, line []Change, excused map[string]bool) error {
	if len(excused) == 0 {
		return nil
	}
	ids := make([]string, 0, len(excused))
	for commit := range excused {
		ids = append(ids, "present("+quoteRev(commit)+")")
	}
	keep := make([]string, 0, 3*len(line))
	for _, change := range line {
		keep = append(keep, quoteRev(change.Tip), "bookmarks(exact:"+quoteRev(change.Bookmark)+")", "remote_bookmarks(exact:"+quoteRev(change.Bookmark)+", remote=exact:\"origin\")")
	}
	rangeRev, _, _ := stackRevs(line)
	rev := "((" + strings.Join(ids, " | ") + ") & descendants(" + quoteRev(root) + ")) ~ (" + rangeRev + ")"
	if len(keep) > 0 {
		rev += " ~ " + strings.Join(keep, " ~ ")
	}
	commits, err := r.revisions(ctx, rev)
	if err != nil {
		return err
	}
	if len(commits) == 0 {
		return nil
	}
	_, err = r.jj(ctx, "abandon", "--ignore-immutable", "-r", rev)
	return err
}

// rebaseFailure adds the recovery commands from syncRecovery to a rebase
// failure. A rebase runs after the line tips are known, so the recovery names
// the old tips and any stale copies, as sync does.
func (r *repository) rebaseFailure(ctx context.Context, plan syncPlan, tipsBefore []string, cause error) error {
	fix, recoveryErr := r.syncRecovery(ctx, plan, tipsBefore)
	if recoveryErr != nil {
		return errors.Join(cause, recoveryErr)
	}
	if f, ok := errors.AsType[*failure](cause); ok {
		failureCopy := *f
		failureCopy.fix += "\n" + fix
		return &failureCopy
	}
	return &failure{stage: "rebase", gate: toolFailureGate, reason: cause.Error(), fix: fix, code: 3, cause: cause}
}

// lineSyncPlan records each segment's tip and its Base..Tip commits before a
// rewrite, so a failed rewrite's recovery abandons every rewritten copy. The
// --at target is recorded like any other bookmark, so it is restored before any
// copy is abandoned.
func (r *repository) lineSyncPlan(ctx context.Context, line []Change) (syncPlan, []string, error) {
	tipsBefore := make([]string, len(line))
	plan := syncPlan{Segments: make([]syncSegment, len(line))}
	for i, change := range line {
		commits, err := r.revisions(ctx, quoteRev(change.Base)+".."+quoteRev(change.Tip))
		if err != nil {
			return syncPlan{}, nil, err
		}
		tipsBefore[i] = change.Tip
		plan.Segments[i] = syncSegment{Bookmark: change.Bookmark, Commits: commits}
	}
	return plan, tipsBefore, nil
}
