package main

import (
	"context"
	"strings"
)

// refuseEmptyChange refuses a submit when a bookmark in the owned line has no
// diff against its fork point. The fork point is heads(::base & ::tip), so a
// base that moved after the stack was cut does not make an empty stack look
// non-empty. A bookmark whose commits are all empty is refused by name, even
// when a child bookmark has changes. A bookmark with some empty commits and a
// real diff still submits.
func (r *repository) refuseEmptyChange(ctx context.Context, line []Change) error {
	for _, change := range line {
		fork := "heads(::" + quoteRev(change.Base) + " & ::" + quoteRev(change.Tip) + ")"
		files, err := r.jj(ctx, "diff", "--name-only", "--from", fork, "--to", quoteRev(change.Tip))
		if err != nil {
			return err
		}
		if files != "" {
			continue
		}
		empty, err := r.revisions(ctx, "("+fork+"..("+quoteRev(change.Tip)+")) & empty() & ~merges()")
		if err != nil {
			return err
		}
		ids := strings.Join(empty, " ")
		if ids == "" {
			ids = "none"
		}
		return refusal("working-copy", "empty-change", change.Bookmark+" has no changes against its fork point; empty commits: "+ids, "jj squash --from <edit commit> --into "+change.Bookmark+", or jj abandon "+ids)
	}
	return nil
}
