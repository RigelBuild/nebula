package main

import (
	"context"
	"strings"
)

type reviewFixPlan struct {
	parent, fix, childRoot string
	line                   []Change
}

// planReviewFix checks both branches before moving any bookmark.
//
//nolint:nilnil // A nil plan means ordinary submit handles this line.
func (r *repository) planReviewFix(ctx context.Context, target, top, selectionTip string) (*reviewFixPlan, error) {
	parent := target
	fix := top
	if fix == "" {
		var err error
		parent, fix, err = r.insertedParentFix(ctx, target)
		if err != nil || fix == "" {
			return nil, err
		}
	}
	old := selectionTip
	if old == "" || parent != target {
		var err error
		old, err = r.one(ctx, quoteRev(parent))
		if err != nil {
			return nil, err
		}
	}
	names, err := r.localBookmarks(ctx, "("+quoteRev(old)+"::) ~ "+quoteRev(old)+" ~ ::"+quoteRev(fix))
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, nil
	}
	last, err := r.reviewFixDescendants(ctx, old, fix, names)
	if err != nil {
		return nil, err
	}
	skipBoundary := ""
	if target != parent {
		skipBoundary = target
	}
	line, err := r.line(ctx, names[len(names)-1], lineOptions{mode: linePreflight, skipOwnership: true, skipBoundary: skipBoundary})
	if err != nil {
		return nil, err
	}
	if err := r.checkReviewFixOwnership(ctx, line, old, fix, last); err != nil {
		return nil, err
	}
	if target == parent {
		children, err := r.revisions(ctx, "roots("+quoteRev(old)+".."+quoteRev(names[0])+")")
		if err != nil {
			return nil, err
		}
		if len(children) != 1 {
			return nil, refusal("ownership", "foreign-descendant", "descendant line has no single root", "separate the descendant before submitting")
		}
		onChild, err := r.revisions(ctx, quoteRev(fix)+" & "+quoteRev(old)+".."+quoteRev(names[0]))
		if err != nil {
			return nil, err
		}
		if len(onChild) != 0 {
			return nil, refusal("ownership", "fix-on-child-line", "review fix lies on the owned child line: "+names[0], "run rjc meta "+parent+` --title "<title>" --body-file <path>`+", then rjc submit "+names[0]+" from its tip")
		}
		return &reviewFixPlan{parent: parent, fix: fix, childRoot: children[0], line: line}, nil
	}
	return &reviewFixPlan{parent: parent, fix: fix, line: line}, nil
}

func (r *repository) reviewFixDescendants(ctx context.Context, old, fix string, names []string) (string, error) {
	insideFix, err := r.localBookmarks(ctx, quoteRev(old)+".."+quoteRev(fix))
	if err != nil {
		return "", err
	}
	if len(insideFix) != 0 {
		return "", refusal("ownership", "foreign-descendant", "review fix crosses another bookmark: "+insideFix[0], "separate the descendant before submitting")
	}
	fixRange := quoteRev(old) + ".." + quoteRev(fix)
	published, err := r.revisions(ctx, "("+fixRange+") & ::remote_bookmarks(remote=exact:\"origin\")")
	if err != nil {
		return "", err
	}
	if len(published) != 0 {
		return "", refusal("ownership", "foreign-published", "review fix includes published work outside the owned bookmarks", "select an unpublished fix")
	}
	last := old
	for _, name := range names {
		if !r.keys.Owned(r.codename, name) {
			return "", refusal("ownership", "foreign-descendant", "rebasing would rewrite a descendant bookmark: "+name, "separate the descendant before submitting")
		}
		tip, err := r.one(ctx, quoteRev(name))
		if err != nil {
			return "", err
		}
		anc, err := r.revisions(ctx, quoteRev(last)+" & ::"+quoteRev(tip))
		if err != nil {
			return "", err
		}
		if len(anc) != 1 || tip == last {
			return "", refusal("ownership", "foreign-descendant", "descendant bookmarks do not form one line", "separate the descendant before submitting")
		}
		last = tip
	}
	return last, nil
}

func (r *repository) checkReviewFixOwnership(ctx context.Context, line []Change, old, fix, last string) error {
	_, roots, descendants := stackRevs(line)
	allowed, err := r.allowedBookmarks(ctx, line, false)
	if err != nil {
		return err
	}
	root, err := r.singleRoot(ctx, roots)
	if err != nil {
		return err
	}
	if err := r.checkForeignPublished(ctx, line, allowed); err != nil {
		return err
	}
	if _, _, err := r.staleRewriteCopies(ctx, line, root, allowed); err != nil {
		return err
	}
	if _, err := r.checkDescendantsExcept(ctx, descendants, allowed, false, fix, nil); err != nil {
		return err
	}
	if err := r.checkDescendants(ctx, "descendants("+quoteRev(fix)+") ~ ::"+quoteRev(last)+" ~ "+quoteRev(fix), allowed); err != nil {
		return err
	}
	bookmarks, err := r.localBookmarks(ctx, roots+"::")
	if err != nil {
		return err
	}
	for _, name := range bookmarks {
		if !allowed[name] {
			return refusal("ownership", "foreign-descendant", "bookmark lies outside the planned line: "+name, "separate the descendant before submitting")
		}
	}
	merges, err := r.revisions(ctx, "merges() & ("+quoteRev(line[0].Base)+".."+quoteRev(last)+" | "+quoteRev(old)+".."+quoteRev(fix)+")")
	if err != nil {
		return err
	}
	if len(merges) != 0 {
		return refusal("ownership", "nonlinear", "owned stack contains a merge", "select a single linear owned stack")
	}
	return nil
}

func (r *repository) insertedParentFix(ctx context.Context, target string) (string, string, error) {
	rel, err := r.syncParent(ctx, target)
	if err != nil || rel == nil || !r.keys.Owned(r.codename, rel.Bookmark) {
		return "", "", err
	}
	landed, _, err := r.parentHasLanded(ctx, *rel)
	if err != nil || landed {
		return "", "", err
	}
	if err := r.requireOpenForkParent(ctx, rel.Bookmark, target); err != nil {
		return "", "", err
	}
	old, err := r.parentRevision(ctx, rel.Bookmark)
	if err != nil || old != rel.Tip {
		return "", "", err
	}
	remote := "remote_bookmarks(exact:" + quoteRev(target) + ", remote=exact:\"origin\")"
	roots, err := r.revisions(ctx, "roots("+quoteRev(rel.Tip)+".."+remote+")")
	if err != nil || len(roots) != 1 {
		return "", "", err
	}
	id, err := r.jj(ctx, "log", "--no-graph", "-r", quoteRev(roots[0]), "-T", "change_id")
	if err != nil {
		return "", "", err
	}
	local, err := r.revisions(ctx, "change_id("+quoteRev(id)+") & ::"+quoteRev(target))
	if err != nil || len(local) != 1 {
		return "", "", err
	}
	fix, err := r.one(ctx, quoteRev(local[0])+"-")
	if err != nil || fix == old {
		return "", "", err
	}
	ancestor, err := r.revisions(ctx, quoteRev(old)+" & ::"+quoteRev(fix))
	if err != nil || len(ancestor) != 1 {
		return "", "", err
	}
	if err := r.refuseReorderedChild(ctx, old, fix, remote); err != nil {
		return "", "", err
	}
	return rel.Bookmark, fix, nil
}

func (r *repository) requireOpenForkParent(ctx context.Context, parent, child string) error {
	if r.profile.Posture == forkNone {
		return nil
	}
	s, err := service(ctx)
	if err != nil {
		return err
	}
	pr, err := s.forge.findPR(ctx, r.profile.Name, parent)
	if err != nil {
		return err
	}
	if pr == 0 {
		return refusal("ownership", "closed-fork-parent", "fork parent has no open PR", "rjc sync "+child)
	}
	return nil
}

func (r *repository) refuseReorderedChild(ctx context.Context, old, fix, remote string) error {
	published, err := r.changeIDs(ctx, quoteRev(old)+".."+remote)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(published))
	for _, id := range published {
		known[id] = true
	}
	ids, err := r.changeIDs(ctx, quoteRev(old)+".."+quoteRev(fix))
	if err != nil {
		return err
	}
	for _, id := range ids {
		if known[id] {
			return refusal("ownership", "parent-boundary", "child work was moved below its published boundary", "restore the child commit order before submitting")
		}
	}
	return nil
}

func (r *repository) applyReviewFix(ctx context.Context, plan *reviewFixPlan) ([]Change, error) {
	recovery, tipsBefore, err := r.lineSyncPlan(ctx, plan.line)
	if err != nil {
		return nil, err
	}
	if err := r.advance(ctx, plan.parent, plan.fix); err != nil {
		return nil, err
	}
	if plan.childRoot != "" {
		if _, err := r.jj(ctx, "rebase", "--ignore-immutable", "-s", quoteRev(plan.childRoot), "-d", quoteRev(plan.parent)); err != nil {
			return nil, err
		}
	}
	for i := range plan.line {
		tip, err := r.one(ctx, quoteRev(plan.line[i].Bookmark))
		if err != nil {
			return nil, err
		}
		plan.line[i].Tip = tip
	}
	conflicts, err := r.revisions(ctx, "conflicts() & ("+quoteRev(plan.line[0].Base)+".."+quoteRev(plan.line[len(plan.line)-1].Tip)+")")
	if err != nil {
		return nil, err
	}
	if len(conflicts) != 0 {
		var output strings.Builder
		for _, commit := range conflicts {
			files, err := r.jj(ctx, "resolve", "--list", "-r", commit)
			if err != nil {
				return nil, err
			}
			output.WriteString(commit + "\n" + files + "\n")
		}
		return nil, r.rebaseFailure(ctx, recovery, tipsBefore, refusal("rebase", "conflict", output.String(), "resolve the conflicts before retrying"))
	}
	if err := r.rebaseOwned(ctx, plan.line, false, nil); err != nil {
		return nil, err
	}
	line, err := r.line(ctx, plan.line[len(plan.line)-1].Bookmark, lineOptions{mode: linePreflight})
	if err != nil {
		return nil, err
	}
	if len(line) != len(plan.line) {
		return nil, refusal("ownership", "foreign-descendant", "rewritten bookmark line changed", "inspect the stack before submitting")
	}
	for i := range line {
		if line[i].Bookmark != plan.line[i].Bookmark || strings.TrimSuffix(line[i].Base, "@origin") != strings.TrimSuffix(plan.line[i].Base, "@origin") {
			return nil, refusal("ownership", "foreign-descendant", "rewritten bookmark line changed", "inspect the stack before submitting")
		}
	}
	return line, nil
}
