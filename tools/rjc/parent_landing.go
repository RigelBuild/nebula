package main

import (
	"context"
	"strings"
)

func (r *repository) parentHasLanded(ctx context.Context, parent parentRelationship) (bool, string, error) {
	if parent.Landed {
		return true, "", nil
	}
	s, err := service(ctx)
	if err != nil {
		return false, "", err
	}
	merged, head, err := s.forge.parentMerged(ctx, r.profile.Name, parent.Bookmark)
	if err != nil || merged {
		return merged, head, err
	}
	reachable, err := r.revisions(ctx, quoteRev(parent.Tip)+" & ::"+quoteRev(r.profile.TrunkRemote()))
	return len(reachable) != 0, "", err
}

func (r *repository) parentWorkRemains(ctx context.Context, parent parentRelationship, mergedHead, tip string) (bool, error) {
	work := "::" + quoteRev(parent.Tip)
	if mergedHead != "" {
		work += " | ::present(" + quoteRev(mergedHead) + ")"
	}
	parentIDs, err := r.changeIDs(ctx, "("+work+") ~ ::"+quoteRev(r.profile.TrunkRemote()))
	if err != nil {
		return false, err
	}
	known := make(map[string]bool, len(parent.ChangeIDs)+len(parentIDs))
	for _, id := range parent.ChangeIDs {
		known[id] = true
	}
	for _, id := range parentIDs {
		known[id] = true
	}
	childIDs, err := r.changeIDs(ctx, quoteRev(r.profile.TrunkRemote())+".."+quoteRev(tip))
	if err != nil {
		return false, err
	}
	for _, id := range childIDs {
		if known[id] {
			return true, nil
		}
	}
	return false, nil
}

func (r *repository) changeIDs(ctx context.Context, rev string) ([]string, error) {
	value, err := r.jj(ctx, "log", "--no-graph", "-r", rev, "-T", `change_id ++ "\n"`)
	if err != nil {
		return nil, err
	}
	return strings.Fields(value), nil
}

func (r *repository) checkLandedBases(line []Change) error {
	if len(r.landedParents) == 0 {
		return nil
	}
	landed := make(map[string]bool, len(r.landedParents))
	for _, parent := range r.landedParents {
		landed[parent] = true
	}
	for _, change := range line {
		parent := strings.TrimSuffix(change.Base, "@origin")
		if landed[parent] {
			return refusal("ownership", "parent-boundary", "landed parent still supplies the bookmark line's base: "+parent, "rjc sync "+change.Bookmark)
		}
	}
	return nil
}
