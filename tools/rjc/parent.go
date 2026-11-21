package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"

	"github.com/samber/oops"
)

type parentRelationship struct {
	Bookmark  string   `json:"bookmark"`
	Tip       string   `json:"tip"`
	ChangeIDs []string `json:"change_ids"`
	Landed    bool     `json:"landed,omitempty"`
}

var fullCommitIDRE = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// selfParentRefusal reports saved parent state that names its own bookmark.
// The state path is printed so the user can move the file aside, which is the
// recovery that worked before this gate existed.
func (r *repository) selfParentRefusal(bookmark string) error {
	path, err := (fileBaseline{root: r.stateDir, keys: r.keys}).path(bookmark, ".parent.json")
	if err != nil {
		return err
	}
	return refusal("ownership", "self-parent", "saved parent names the bookmark itself: "+bookmark, "move "+path+" aside, then rerun")
}

func (r *repository) validParentBookmark(bookmark string) bool {
	return bookmark != "" && bookmark != r.profile.Trunk && !strings.ContainsAny(bookmark, "@\r\n")
}

func (r *repository) validateParentState(ctx context.Context, parent parentRelationship, bookmark string) error {
	if !r.validParentBookmark(parent.Bookmark) {
		return oops.Errorf("invalid selected parent bookmark")
	}
	if parent.Bookmark == bookmark {
		return r.selfParentRefusal(bookmark)
	}
	if !fullCommitIDRE.MatchString(parent.Tip) {
		return oops.Errorf("selected parent tip must be a full 40-hex commit ID")
	}
	resolved, err := r.revisions(ctx, quoteRev(parent.Tip))
	if err != nil {
		return oops.Wrapf(err, "resolve selected parent tip")
	}
	if len(resolved) != 1 || !strings.EqualFold(resolved[0], parent.Tip) {
		return oops.Errorf("selected parent tip must resolve to its exact commit")
	}
	return nil
}

func (r *repository) saveParent(ctx context.Context, bookmark string, parent parentRelationship) error {
	if parent.Bookmark == bookmark {
		return r.selfParentRefusal(bookmark)
	}
	path, err := (fileBaseline{root: r.stateDir, keys: r.keys}).path(bookmark, ".parent.json")
	if err != nil {
		return err
	}
	currentIDs, err := r.changeIDs(ctx, quoteRev(r.profile.TrunkRemote())+".."+quoteRev(parent.Tip))
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(parent.ChangeIDs)+len(currentIDs))
	for _, id := range parent.ChangeIDs {
		known[id] = true
	}
	for _, id := range currentIDs {
		if !known[id] {
			parent.ChangeIDs = append(parent.ChangeIDs, id)
			known[id] = true
		}
	}
	data, err := json.Marshal(parent)
	if err != nil {
		return oops.Wrapf(err, "encode selected parent")
	}
	return atomicWrite(path, string(data))
}

func (r *repository) clearParent(bookmark string) error {
	path, err := (fileBaseline{root: r.stateDir, keys: r.keys}).path(bookmark, ".parent.json")
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return oops.Wrapf(err, "clear selected parent")
	}
	return nil
}

func (r *repository) refreshParentBoundary(ctx context.Context, bookmark, tip string, mode lineMode) error {
	path, err := (fileBaseline{root: r.stateDir, keys: r.keys}).path(bookmark, ".parent.json")
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path) //nolint:gosec // State paths are validated by fileBaseline.path.
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return oops.Wrapf(err, "read selected parent")
	}
	var selected parentRelationship
	if err := json.Unmarshal(data, &selected); err != nil {
		return oops.Wrapf(err, "decode selected parent")
	}
	if selected.Bookmark == "" || selected.Tip == "" {
		return oops.Errorf("selected parent state is incomplete")
	}
	if err := r.validateParentState(ctx, selected, bookmark); err != nil {
		return err
	}
	landed, mergedHead, err := r.parentHasLanded(ctx, selected)
	if err != nil {
		return err
	}
	if landed {
		remaining, err := r.parentWorkRemains(ctx, selected, mergedHead, tip)
		if err != nil {
			return err
		}
		if remaining {
			return refusal("ownership", "parent-boundary", "landed parent work remains in the child: "+selected.Bookmark, "rjc sync "+bookmark)
		}
		if r.landedParents == nil {
			r.landedParents = make(map[string]string)
		}
		r.landedParents[bookmark] = selected.Bookmark
		return nil
	}
	if r.profile.Posture != forkNone {
		s, err := service(ctx)
		if err != nil {
			return err
		}
		pr, err := s.forge.findPR(ctx, r.profile.Name, selected.Bookmark)
		if err != nil {
			return err
		}
		if pr == 0 {
			return refusal("ownership", "closed-fork-parent", "fork parent has no open PR", "rjc sync "+bookmark)
		}
	}
	base := "remote_bookmarks(exact:" + quoteRev(selected.Bookmark) + ", exact:\"origin\")"
	if r.keys.Owned(r.codename, selected.Bookmark) {
		local, err := r.revisions(ctx, "bookmarks(exact:"+quoteRev(selected.Bookmark)+")")
		if err != nil {
			return err
		}
		if len(local) == 1 {
			base = quoteRev(local[0])
		}
	}
	current, err := r.revisions(ctx, base)
	if err != nil {
		return err
	}
	invalid := func() error {
		return refusal("ownership", "parent-boundary", "selected parent no longer forms a valid boundary: "+selected.Bookmark, "rjc sync "+bookmark)
	}
	if len(current) != 1 {
		return invalid()
	}
	ancestors, err := r.revisions(ctx, base+" & ::"+quoteRev(tip))
	if err != nil {
		return err
	}
	if len(ancestors) != 1 || current[0] == tip {
		return invalid()
	}
	if current[0] == selected.Tip {
		return nil
	}
	oldOutsideBase, err := r.revisions(ctx, quoteRev(selected.Tip)+" & (::"+quoteRev(tip)+" ~ ::"+base+")")
	if err != nil {
		return err
	}
	if len(oldOutsideBase) != 0 {
		return invalid()
	}
	if mode == linePreflight {
		return nil
	}
	selected.Tip = current[0]
	return r.saveParent(ctx, bookmark, selected)
}
