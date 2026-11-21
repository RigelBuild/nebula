package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"

	"github.com/samber/oops"
)

type syncOptions struct{ bookmark string }
type syncSegment struct {
	Bookmark string
	Parent   string
	Root     string
	Commits  []string
}
type syncPlan struct {
	StopBookmark string
	Stop         *parentRelationship
	Destination  string
	Segments     []syncSegment
	Stale        []string
	Reparent     []syncReparent
}

// syncReparent records a child whose saved parent was stale. Bookmark now keeps
// Parent, the owned bookmark inserted between Saved and Bookmark. Saved is the
// old state, kept so the new state keeps its change IDs.
type syncReparent struct {
	Bookmark string
	Parent   string
	Saved    parentRelationship
}
type SyncResult struct {
	Bookmark string `json:"bookmark"`
	Base     string `json:"base"`
	Tip      string `json:"tip"`
}

//nolint:nilnil // A missing state file represents no saved parent.
func (r *repository) syncParent(ctx context.Context, bookmark string) (*parentRelationship, error) {
	path, err := (fileBaseline{root: r.stateDir, keys: r.keys}).path(bookmark, ".parent.json")
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // path validates the bookmark.
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, oops.Wrapf(err, "read selected parent")
	}
	var parent parentRelationship
	if err := json.Unmarshal(data, &parent); err != nil {
		return nil, oops.Wrapf(err, "decode selected parent")
	}
	if err := r.validateParentState(ctx, parent, bookmark); err != nil {
		return nil, err
	}
	return &parent, nil
}

// parentRevision resolves an owned parent to its local bookmark. Only when no
// local bookmark exists does it fall back to parent@origin; a conflicted local
// bookmark is refused as ambiguous rather than hidden behind origin's copy.
// Unlike ownedParentBase, it never prefers origin over an existing local copy.
func (r *repository) parentRevision(ctx context.Context, bookmark string) (string, error) {
	local := "bookmarks(exact:" + quoteRev(bookmark) + ")"
	found, err := r.revisions(ctx, local)
	if err != nil {
		return "", err
	}
	if len(found) == 0 {
		return r.one(ctx, quoteRev(bookmark)+"@origin")
	}
	return r.one(ctx, local)
}

// hasLocalBookmark reports whether bookmark has a local copy.
func (r *repository) hasLocalBookmark(ctx context.Context, bookmark string) (bool, error) {
	found, err := r.revisions(ctx, "bookmarks(exact:"+quoteRev(bookmark)+")")
	return len(found) > 0, err
}

// refuseConflictedBookmark refuses a conflicted local bookmark as ambiguous.
// Resolving its revset directly fails with jj's raw "is conflicted" error; this
// names the rjc gate and the recovery instead.
func (r *repository) refuseConflictedBookmark(ctx context.Context, bookmark string) error {
	state, err := r.jj(ctx, "bookmark", "list", "exact:"+bookmark, "-T", `if(!remote, if(conflict, "conflict", "normal"))`)
	if err != nil {
		return err
	}
	if strings.TrimSpace(state) != "conflict" {
		return nil
	}
	return refusal("working-copy", "ambiguous-revision", "bookmark is conflicted locally: "+bookmark, "resolve it with jj bookmark set "+bookmark+" -r <commit>, or jj bookmark forget "+bookmark+" if origin's copy is correct")
}

// refuseLocalBelowOriginOnly refuses an origin-only parent with a local owned
// bookmark below it. Sync would rebase the child onto origin's copy, and line
// would then place that local bookmark under the child, which submit refuses
// as foreign-published. Refusing here keeps sync from rewriting first.
func (r *repository) refuseLocalBelowOriginOnly(ctx context.Context, parent string) error {
	below, err := r.localBookmarks(ctx, "::"+quoteRev(parent+"@origin")+" ~ ::"+quoteRev(r.profile.TrunkRemote()))
	if err != nil {
		return err
	}
	for _, name := range below {
		if r.keys.Owned(r.codename, name) {
			return refusal("ownership", "foreign-published", "owned bookmark "+name+" sits below origin-only parent "+parent, "select a base after the published work")
		}
	}
	return nil
}

func syncSet(commits []string) string {
	if len(commits) == 0 {
		return "none()"
	}
	quoted := make([]string, len(commits))
	for i, commit := range commits {
		quoted[i] = quoteRev(commit)
	}
	return "(" + strings.Join(quoted, " | ") + ")"
}

// insertedParent finds the owned bookmark that a later insertion put between a
// saved parent and bookmark. It returns nil when no owned bookmark sits in that
// range, so the saved parent is not stale; the saved parent may also have landed.
// A single owned intermediate saved on the saved parent is the new parent. A
// single intermediate with no saved parent, or saved elsewhere, refuses as
// unlinked-parent. Several intermediates refuse as ambiguous-parent, so sync
// never guesses the stack.
//
//nolint:nilnil // nil means the saved parent is not stale.
func (r *repository) insertedParent(ctx context.Context, bookmark, tip string, saved parentRelationship) (*parentRelationship, error) {
	between, err := r.localBookmarks(ctx, "("+quoteRev(saved.Tip)+"::"+quoteRev(tip)+") ~ "+quoteRev(saved.Tip)+" ~ "+quoteRev(tip))
	if err != nil {
		return nil, err
	}
	var all, owned []string
	for _, name := range between {
		if name == bookmark || name == saved.Bookmark {
			continue
		}
		all = append(all, name)
		if r.keys.Owned(r.codename, name) {
			owned = append(owned, name)
		}
	}
	if len(owned) == 0 {
		return nil, nil
	}
	if len(all) == 1 {
		state, err := r.syncParent(ctx, owned[0])
		if err != nil {
			return nil, err
		}
		if state != nil && state.Bookmark == saved.Bookmark {
			tipID, err := r.one(ctx, quoteRev(owned[0]))
			if err != nil {
				return nil, err
			}
			return &parentRelationship{Bookmark: owned[0], Tip: tipID}, nil
		}
		reason := owned[0] + " sits between saved parent " + saved.Bookmark + " and " + bookmark + " but has no saved parent"
		if state != nil {
			reason = owned[0] + " sits between saved parent " + saved.Bookmark + " and " + bookmark + " but is saved on " + state.Bookmark
		}
		return nil, refusal("ownership", "unlinked-parent", reason, "retire "+owned[0]+" with `rjc retire` if its PR is closed or merged; no rjc command sets a saved parent on an open bookmark yet, so this sync stops here")
	}
	return nil, refusal("ownership", "ambiguous-parent", "bookmarks sit between saved parent "+saved.Bookmark+" and "+bookmark+": "+strings.Join(all, ", "), "retire the closed or merged ones with `rjc retire` until one remains, then rerun rjc sync "+bookmark)
}

// reparentSaved saves each planned reparent after the line validates, so a
// refused sync leaves the saved state as it was. The new parent is recorded at
// its tip after the rebase.
func (r *repository) reparentSaved(ctx context.Context, reparents []syncReparent) error {
	for _, reparent := range reparents {
		tip, err := r.one(ctx, quoteRev(reparent.Parent))
		if err != nil {
			return err
		}
		updated := reparent.Saved
		updated.Bookmark = reparent.Parent
		updated.Tip = tip
		if err := r.saveParent(ctx, reparent.Bookmark, updated); err != nil {
			return err
		}
	}
	return nil
}

//nolint:cyclop,nestif // Keep the parent walk and its boundary decisions together.
func (r *repository) planSync(ctx context.Context, bookmark string) (syncPlan, error) {
	plan := syncPlan{Destination: r.profile.TrunkRemote(), Segments: []syncSegment{}, Stale: []string{}}
	seen := map[string]bool{}
	for current := bookmark; ; {
		if !r.keys.Owned(r.codename, current) || seen[current] {
			return plan, refusal("ownership", "outside-line", "invalid owned parent line: "+current, "select one owned line")
		}
		seen[current] = true
		if err := r.refuseConflictedBookmark(ctx, current); err != nil {
			return plan, err
		}
		tip, err := r.one(ctx, quoteRev(current))
		if err != nil {
			return plan, err
		}
		parent, err := r.syncParent(ctx, current)
		if err != nil {
			return plan, err
		}
		// A landed parent stops the walk, so the origin-only check below does not
		// apply to it. Otherwise an owned parent with no local bookmark cannot be
		// rebased as a segment; its origin copy is the boundary, like an unowned parent.
		landed := false
		if parent != nil {
			landed, _, err = r.parentHasLanded(ctx, *parent)
			if err != nil {
				return plan, err
			}
		}
		// An inserted owned bookmark replaces the saved parent, even when that parent
		// landed. Its own walk then stops at the landed parent.
		if parent != nil && r.keys.Owned(r.codename, parent.Bookmark) {
			inserted, err := r.insertedParent(ctx, current, tip, *parent)
			if err != nil {
				return plan, err
			}
			if inserted != nil {
				plan.Reparent = append(plan.Reparent, syncReparent{Bookmark: current, Parent: inserted.Bookmark, Saved: *parent})
				parent = inserted
				landed, _, err = r.parentHasLanded(ctx, *parent)
				if err != nil {
					return plan, err
				}
			}
		}
		originOnly := false
		if parent != nil && !landed && r.keys.Owned(r.codename, parent.Bookmark) {
			hasLocal, err := r.hasLocalBookmark(ctx, parent.Bookmark)
			if err != nil {
				return plan, err
			}
			originOnly = !hasLocal
		}
		segment := syncSegment{Bookmark: current, Commits: []string{}}
		base := "::" + quoteRev(r.profile.TrunkRemote())
		if parent != nil {
			if landed || !r.keys.Owned(r.codename, parent.Bookmark) || originOnly {
				if landed {
					plan.Stop = parent
					plan.StopBookmark = current
				} else {
					if originOnly {
						if err := r.refuseLocalBelowOriginOnly(ctx, parent.Bookmark); err != nil {
							return plan, err
						}
					}
					plan.Destination = parent.Bookmark + "@origin"
					if r.profile.Posture != forkNone {
						s, err := service(ctx)
						if err != nil {
							return plan, err
						}
						pr, err := s.forge.findPR(ctx, r.profile.Name, parent.Bookmark)
						if err != nil {
							return plan, err
						}
						if pr == 0 {
							return plan, refusal("ownership", "closed-fork-parent", "fork parent has no open PR", "select an open parent")
						}
					}
				}
				base += " | ::" + quoteRev(parent.Tip)
			} else {
				segment.Parent = parent.Bookmark
				parentRev, err := r.parentRevision(ctx, parent.Bookmark)
				if err != nil {
					return plan, err
				}
				remote, err := r.revisions(ctx, "remote_bookmarks(exact:"+quoteRev(parent.Bookmark)+", remote=exact:\"origin\")")
				if err != nil {
					return plan, err
				}
				if len(remote) > 0 && (len(remote) != 1 || remote[0] != parentRev) {
					return plan, refusal("ownership", "parent-diverged", "published parent differs from origin: "+parent.Bookmark, "reconcile the parent before sync")
				}
				base += " | ::" + quoteRev(parentRev) + " | ::" + quoteRev(parent.Tip)
			}
		}
		rev := "::" + quoteRev(tip) + " ~ (" + base + ")"
		commits, err := r.revisions(ctx, rev)
		if err != nil {
			return plan, err
		}
		if originOnly {
			if err := r.refuseDivergentOriginOnlyParent(ctx, parent.Bookmark, commits); err != nil {
				return plan, err
			}
		}
		if parent != nil {
			known := map[string]bool{}
			if segment.Parent != "" {
				parentRev, err := r.parentRevision(ctx, segment.Parent)
				if err != nil {
					return plan, err
				}
				ids, err := r.changeIDs(ctx, quoteRev(r.profile.TrunkRemote())+".."+quoteRev(parentRev))
				if err != nil {
					return plan, err
				}
				for _, id := range ids {
					known[id] = true
				}
				oldCopies, err := r.revisions(ctx, "(::"+quoteRev(tip)+" ~ ::"+quoteRev(parentRev)+" ~ ::"+quoteRev(r.profile.TrunkRemote())+") & ::"+quoteRev(parent.Tip))
				if err != nil {
					return plan, err
				}
				for _, commit := range oldCopies {
					id, err := r.jj(ctx, "log", "--no-graph", "-r", quoteRev(commit), "-T", "change_id")
					if err != nil {
						return plan, err
					}
					if known[id] {
						plan.Stale = append(plan.Stale, commit)
					}
				}
			}
			for _, commit := range commits {
				id, err := r.jj(ctx, "log", "--no-graph", "-r", quoteRev(commit), "-T", "change_id")
				if err != nil {
					return plan, err
				}
				if known[id] {
					plan.Stale = append(plan.Stale, commit)
				} else {
					segment.Commits = append(segment.Commits, commit)
				}
			}
		} else {
			segment.Commits = commits
		}
		roots, err := r.revisions(ctx, "roots("+syncSet(segment.Commits)+")")
		if err != nil {
			return plan, err
		}
		if len(roots) != 1 || !slices.Contains(segment.Commits, tip) {
			return plan, refusal("ownership", "outside-line", "segment is not a single rooted owned line: "+current, "separate the selected line")
		}
		segment.Root = roots[0]
		plan.Segments = append(plan.Segments, segment)
		if segment.Parent == "" {
			break
		}
		current = segment.Parent
	}
	slices.Reverse(plan.Segments)
	return plan, nil
}

// refuseDivergentOriginOnlyParent refuses when a segment commit has the same
// change ID as a commit of parent@origin. It compares change IDs only, not
// commit IDs. Such a copy is the parent's amended local rewrite; rebasing it
// would leave the change divergent and the next submit would refuse it.
func (r *repository) refuseDivergentOriginOnlyParent(ctx context.Context, parent string, commits []string) error {
	originIDs, err := r.changeIDs(ctx, quoteRev(r.profile.TrunkRemote())+".."+quoteRev(parent+"@origin"))
	if err != nil {
		return err
	}
	origin := make(map[string]bool, len(originIDs))
	for _, id := range originIDs {
		origin[id] = true
	}
	for _, commit := range commits {
		id, err := r.jj(ctx, "log", "--no-graph", "-r", quoteRev(commit), "-T", "change_id")
		if err != nil {
			return err
		}
		if origin[id] {
			return refusal("ownership", "divergent-line", "local copy of origin-only parent diverges from origin: "+parent+" change "+id, "reconcile the local copy, then retry sync")
		}
	}
	return nil
}
