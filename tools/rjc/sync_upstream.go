package main

import (
	"context"
	"errors"
	"fmt"
	"io"
)

func (r *repository) syncUpstreamMerge(ctx context.Context, bookmark string, log io.Writer, rec UpstreamRecord) ([]SyncResult, error) {
	tip, err := r.one(ctx, quoteRev(bookmark))
	if err != nil {
		return nil, err
	}
	line := []Change{{Bookmark: bookmark, Tip: tip, Base: r.profile.TrunkRemote(), Owned: true}}
	merges := []string{tip}
	allowed, err := r.allowedMergeWithRecord(ctx, line, merges, rec)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, refusal("ownership", "nonlinear", "sync line contains an invalid merge", "select a recorded upstream merge")
	}
	if err := r.checkDescendants(ctx, "descendants("+quoteRev(tip)+") ~ "+quoteRev(tip), nil); err != nil {
		return nil, err
	}
	trunk, err := r.one(ctx, quoteRev(r.profile.TrunkRemote()))
	if err != nil {
		return nil, err
	}
	oldTip := tip
	moved := false
	fail := func(cause error) error {
		if !moved {
			return cause
		}
		plan := syncPlan{Segments: []syncSegment{{Bookmark: bookmark, Commits: []string{oldTip}}}}
		fix, recoveryErr := r.syncRecovery(ctx, plan, []string{oldTip})
		if recoveryErr != nil {
			return errors.Join(cause, recoveryErr)
		}
		if detail, ok := errors.AsType[*failure](cause); ok {
			failureCopy := *detail
			failureCopy.fix += "\n" + fix
			return &failureCopy
		}
		return &failure{stage: "sync", gate: toolFailureGate, reason: cause.Error(), fix: fix, code: 3, cause: cause}
	}
	parents, err := r.jj(ctx, "log", "--no-graph", "-r", quoteRev(tip), "-T", `parents.map(|p| p.commit_id()).join("\n")`)
	if err != nil {
		return nil, err
	}
	if parents != trunk+"\n"+rec.Upstream {
		if _, err := r.jj(ctx, "rebase", "--ignore-immutable", "-s", tip, "-d", trunk, "-d", rec.Upstream); err != nil {
			return nil, err
		}
		moved = true
	}
	tip, err = r.one(ctx, quoteRev(bookmark))
	if err != nil {
		return nil, fail(err)
	}
	conflicts, err := r.revisions(ctx, "conflicts() & "+quoteRev(tip))
	if err != nil {
		return nil, fail(err)
	}
	if len(conflicts) != 0 {
		files, err := r.jj(ctx, "resolve", "--list", "-r", tip)
		if err != nil {
			return nil, fail(err)
		}
		return nil, fail(refusal("rebase", "conflict", "conflicted merge: "+files, "resolve in the child and jj squash into the merge"))
	}
	if _, err := r.line(ctx, bookmark, lineOptions{}); err != nil {
		return nil, fail(err)
	}
	if _, err := fmt.Fprintf(log, "%s %s\n", bookmark, tip); err != nil {
		return nil, fail(err)
	}
	return []SyncResult{{Bookmark: bookmark, Base: r.profile.TrunkRemote(), Tip: tip}}, nil
}
