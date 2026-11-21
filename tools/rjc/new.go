package main

import (
	"context"
	"slices"
	"strings"

	"github.com/samber/oops"
)

type newOptions struct {
	issue, title, on, at string
	issues               []string
}

// parseIssues uses primary when no explicit issue values are supplied.
func parseIssues(keys issueKeys, values []string, primary string) ([]int, error) {
	if len(values) == 0 {
		values = []string{primary}
	}
	issues := make([]int, 0, len(values))
	for _, value := range values {
		issue, err := keys.issueNumber(value)
		if err != nil {
			return nil, err
		}
		if slices.Contains(issues, issue) {
			return nil, refusal("gate-state", "duplicate-issue", "duplicate --issue value", "provide each issue only once")
		}
		issues = append(issues, issue)
	}
	return issues, nil
}

func (r *repository) newChange(ctx context.Context, opt newOptions) (string, error) {
	issues, err := parseIssues(r.keys, opt.issues, opt.issue)
	if err != nil {
		return "", err
	}
	issue := issues[0]
	title, err := r.keys.FullTitle(opt.title, issues)
	if err != nil {
		return "", err
	}
	bookmark, err := r.keys.BookmarkName(r.codename, issue, title)
	if err != nil {
		return "", err
	}
	if err := r.clean(ctx); err != nil {
		return "", err
	}
	existing, err := r.revisions(ctx, "bookmarks(exact:"+quoteRev(bookmark)+")")
	if err != nil {
		return "", err
	}
	if len(existing) != 0 {
		return "", refusal("new", "bookmark-exists", bookmark+" already exists", "use rjc submit "+bookmark+" for existing work")
	}
	if opt.on == bookmark {
		return "", refusal("new", "self-parent", "selected parent names the bookmark itself: "+bookmark, "use --on <another bookmark>, or omit --on")
	}
	base, err := r.newBase(ctx, opt.on)
	if err != nil {
		return "", err
	}
	baseID, err := r.one(ctx, base)
	if err != nil {
		return "", err
	}
	tip := "@"
	if opt.at == "" {
		if _, err := r.jj(ctx, "new", baseID, "-m", title+"\n\nCo-authored-by: "+r.profile.CoAuthor); err != nil {
			return "", err
		}
	} else {
		root, at, planErr := r.adoptionPlan(ctx, opt.at, baseID)
		if planErr != nil {
			return "", planErr
		}
		if err := r.refuseDivergentLine(ctx, []Change{{Base: baseID, Tip: at}}); err != nil {
			return "", err
		}
		tip, err = r.retitleRoot(ctx, root, at, title)
		if err != nil {
			return "", err
		}
	}
	changeID, err := r.jj(ctx, "log", "--no-graph", "-r", tip, "-T", "change_id")
	if err != nil {
		return "", err
	}
	if opt.at != "" {
		line := []Change{{Bookmark: bookmark, Tip: tip, Base: baseID, Issue: issue, Owned: true}}
		if err := r.rebaseOwned(ctx, line, false, nil); err != nil {
			return "", err
		}
	}
	if _, err := r.jj(ctx, "bookmark", "create", bookmark, "-r", quoteRev(changeID)); err != nil {
		return "", err
	}
	if opt.on != "" {
		err = r.saveParent(ctx, bookmark, parentRelationship{Bookmark: opt.on, Tip: baseID})
	} else {
		err = r.clearParent(bookmark)
	}
	if err != nil {
		return "", err
	}
	if err := r.saveIssues(bookmark, issues); err != nil {
		return "", err
	}
	if _, err := r.jj(ctx, "new", quoteRev(bookmark)); err != nil {
		return "", err
	}
	return bookmark, nil
}

func (r *repository) newBase(ctx context.Context, on string) (string, error) {
	if on == "" {
		return r.profile.TrunkRemote(), nil
	}
	if !r.validParentBookmark(on) {
		return "", refusal("new", "invalid-parent", "--on requires an exact parent bookmark name", "use --on <bookmark>, without @origin")
	}
	if _, exists, err := (fileBaseline{root: r.stateDir, keys: r.keys}).LoadUpstream(on); err != nil {
		return "", err
	} else if exists {
		return "", refusal("new", "nonlinear", "cannot stack on an upstream-sync bookmark", "submit the upstream merge without stacked work")
	}
	if r.profile.Posture != forkNone {
		if err := prepareForge(ctx, r); err != nil {
			return "", err
		}
		s, err := service(ctx)
		if err != nil {
			return "", err
		}
		pr, err := s.forge.findPR(ctx, r.profile.Name, on)
		if err != nil {
			return "", err
		}
		if pr == 0 {
			return "", refusal("new", "closed-fork-parent", "fork parent has no open PR", "select an open parent PR")
		}
	}
	if r.keys.Owned(r.codename, on) {
		return r.ownedParentBase(ctx, on)
	}
	return quoteRev(on) + "@origin", nil
}

func (r *repository) ownedParentBase(ctx context.Context, on string) (string, error) {
	local, err := r.revisions(ctx, "bookmarks(exact:"+quoteRev(on)+")")
	if err != nil {
		return "", err
	}
	remote, err := r.revisions(ctx, "remote_bookmarks(exact:"+quoteRev(on)+", remote=exact:\"origin\")")
	if err != nil {
		return "", err
	}
	if len(local) != 1 {
		return quoteRev(on) + "@origin", nil
	}
	if len(remote) == 0 {
		return quoteRev(on), nil
	}
	// Base on local X when origin's copy is its ancestor: X@origin would put X's
	// unpushed commits in the adopted range and refuse as existing-bookmark.
	covered, err := r.revisions(ctx, quoteRev(remote[0])+" & ::bookmarks(exact:"+quoteRev(on)+")")
	if err != nil {
		return "", err
	}
	if len(covered) == 0 {
		return quoteRev(on) + "@origin", nil
	}
	return quoteRev(on), nil
}

func (r *repository) adoptionPlan(ctx context.Context, revision, base string) (rootID, tipID string, err error) {
	at, err := r.one(ctx, revision)
	if err != nil {
		return "", "", err
	}
	rangeRev, rootsRev, descendants := stackRevs([]Change{{Tip: at, Base: base}})
	commits, err := r.revisions(ctx, rangeRev)
	if err != nil {
		return "", "", err
	}
	if len(commits) == 0 {
		return "", "", refusal("new", "wrong-base", "--at has no work above the selected base", "select committed work above the selected base")
	}
	bookmarks, err := r.localBookmarks(ctx, rangeRev)
	if err != nil {
		return "", "", err
	}
	if len(bookmarks) > 0 {
		return "", "", refusal("new", "existing-bookmark", "--at reaches another bookmark", "submit the existing bookmark instead")
	}
	published, err := r.revisions(ctx, "("+rangeRev+") & ::remote_bookmarks(remote=exact:\"origin\")")
	if err != nil {
		return "", "", err
	}
	if len(published) != 0 {
		return "", "", refusal("new", "remote-parent", "--at reaches origin-published work above the selected base", "rjc new --on <bookmark> --at <rev> to select and record the parent explicitly")
	}
	root, err := r.singleRoot(ctx, rootsRev)
	if err != nil {
		return "", "", err
	}
	merges, err := r.revisions(ctx, "merges() & "+rangeRev)
	if err != nil {
		return "", "", err
	}
	if len(merges) > 0 {
		return "", "", refusal("new", "nonlinear", "--at contains a merge", "select a single linear change")
	}
	if err := r.checkAdoptionPRs(ctx, rangeRev); err != nil {
		return "", "", err
	}
	// In-range bookmarks are refused above; descendants still need a separate check.
	if err := r.checkDescendants(ctx, descendants, nil); err != nil {
		return "", "", err
	}
	return root, at, nil
}

func (r *repository) retitleRoot(ctx context.Context, root, at, title string) (string, error) {
	description, err := r.jj(ctx, "log", "--no-graph", "-r", root, "-T", "description")
	if err != nil {
		return "", err
	}
	_, body, _ := strings.Cut(description, "\n")
	if !strings.Contains(body, "Co-authored-by:") {
		body = strings.TrimSpace(body) + "\n\nCo-authored-by: " + r.profile.CoAuthor
	}
	changeID, err := r.jj(ctx, "log", "--no-graph", "-r", at, "-T", "change_id")
	if err != nil {
		return "", err
	}
	if _, err := r.jj(ctx, "describe", "-r", root, "-m", title+"\n\n"+strings.TrimSpace(body)); err != nil {
		return "", err
	}
	return r.one(ctx, changeID)
}

func (r *repository) localBookmarks(ctx context.Context, rev string) ([]string, error) {
	value, err := r.jj(ctx, "log", "--no-graph", "--reversed", "-r", rev, "-T", `local_bookmarks.map(|b| b.name()).join("\n") ++ "\n"`)
	if err != nil {
		return nil, err
	}
	if value == "" {
		return []string{}, nil
	}
	return strings.Fields(value), nil
}

func (r *repository) stackBookmarks(ctx context.Context, rev string) ([]string, error) {
	value, err := r.jj(ctx, "log", "--no-graph", "--reversed", "-r", rev, "-T", `remote_bookmarks.filter(|b| b.remote() == "origin").map(|b| b.name() ++ "@origin").join("\n") ++ "\n" ++ local_bookmarks.map(|b| b.name()).join("\n") ++ "\n"`)
	if err != nil {
		return nil, err
	}
	return strings.Fields(value), nil
}

type lineMode uint8

const (
	lineRecord lineMode = iota
	linePreflight
)

type lineOptions struct {
	topTip       string
	selectionTip string
	mode         lineMode
	// atRetarget is set only by submit --at: a local owned chain above the selected tip may stay.
	atRetarget bool
	// skipOwnership: the caller checks ownership on an extended line (submit --at preflight).
	skipOwnership bool
	// reviewOnly: note and ready rewrite nothing, so the line skips rewrite-safety checks (checkRewrite, checkDescendants). Bookmark ownership in line() still applies.
	reviewOnly bool
	// skipBoundary is validated against the planned parent after the restack.
	skipBoundary string
}

func (r *repository) line(ctx context.Context, target string, opt lineOptions) ([]Change, error) {
	tip := opt.selectionTip
	if tip == "" {
		var err error
		tip, err = r.one(ctx, quoteRev(target))
		if err != nil {
			return nil, err
		}
	}
	rangeRev, err := r.lineRange(ctx, target, tip)
	if err != nil {
		return nil, err
	}
	bookmarks, err := r.stackBookmarks(ctx, rangeRev)
	if err != nil {
		return nil, err
	}
	line := []Change{}
	base := r.profile.TrunkRemote()
	seen := map[string]bool{}
	for _, bookmark := range bookmarks {
		name := strings.TrimSuffix(bookmark, "@origin")
		if strings.HasSuffix(bookmark, "@origin") && r.keys.Owned(r.codename, name) {
			originOnly, err := r.originOnlyBase(ctx, name, len(line) > 0 || seen[name])
			if err != nil {
				return nil, err
			}
			if originOnly {
				seen[name] = true
				base = bookmark
			}
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		bookmark = name
		if bookmark == r.profile.Trunk {
			continue
		}
		if !r.keys.Owned(r.codename, bookmark) {
			if len(line) > 0 {
				return nil, refusal("ownership", "foreign-bookmark", "another caller's bookmark interrupts the line: "+bookmark, "submit only a line based on "+bookmark+"@origin")
			}
			remote, err := r.revisions(ctx, "remote_bookmarks(exact:"+quoteRev(bookmark)+", remote=exact:\"origin\")")
			if err != nil {
				return nil, err
			}
			if len(remote) == 0 {
				return nil, refusal("ownership", "foreign-bookmark", "another caller's bookmark has no origin: "+bookmark, "submit only a line based on a published bookmark")
			}
			base = bookmark + "@origin"
			continue
		}
		issue, err := r.keys.bookmarkIssue(bookmark)
		if err != nil {
			return nil, err
		}
		head := tip
		if bookmark != target || opt.selectionTip == "" {
			head, err = r.one(ctx, quoteRev(bookmark))
			if err != nil {
				return nil, err
			}
		}
		if err := r.refreshLineBoundary(ctx, bookmark, head, opt); err != nil {
			return nil, err
		}
		if len(line) > 0 && line[len(line)-1].Tip == head {
			return nil, oops.Errorf("multiple bookmarks point at one revision; select an unambiguous line")
		}
		line = append(line, Change{Bookmark: bookmark, Tip: head, Base: base, Issue: issue, Owned: true})
		base = bookmark
	}
	if len(line) == 0 || line[len(line)-1].Bookmark != target {
		return nil, oops.Errorf("target is not the top owned bookmark on its line")
	}
	if err := r.checkLandedBases(line); err != nil {
		return nil, err
	}
	if opt.topTip != "" {
		line[len(line)-1].Tip = opt.topTip
	}
	if !opt.skipOwnership {
		if err := r.checkStackOwnership(ctx, line, opt.atRetarget, opt.reviewOnly); err != nil {
			return nil, err
		}
	}
	return line, nil
}

// originOnlyBase reports whether an owned origin bookmark below the line must
// be its base: the parent's local bookmark is gone, so only origin's copy
// remains. A local bookmark, when present, carries the line instead.
func (r *repository) originOnlyBase(ctx context.Context, name string, placed bool) (bool, error) {
	if placed {
		return false, nil
	}
	hasLocal, err := r.hasLocalBookmark(ctx, name)
	return !hasLocal, err
}

func (r *repository) refreshLineBoundary(ctx context.Context, bookmark, head string, opt lineOptions) error {
	if bookmark == opt.skipBoundary {
		return nil
	}
	return r.refreshParentBoundary(ctx, bookmark, head, opt.mode)
}

func (r *repository) lineRange(ctx context.Context, target, tip string) (string, error) {
	rangeRev := quoteRev(r.profile.TrunkRemote()) + ".." + quoteRev(tip)
	if r.profile.Posture == forkTracking && r.profile.Mirror != "" {
		_, recorded, err := r.recordedUpstreamMerge(ctx, target, tip)
		if err != nil {
			return "", err
		}
		if recorded {
			rangeRev += " ~ ::" + quoteRev(r.profile.Mirror+"@origin")
		}
	}
	return rangeRev, nil
}

func stackRevs(line []Change) (rangeRev, roots, descendants string) {
	rangeRev = quoteRev(line[0].Base) + ".." + quoteRev(line[len(line)-1].Tip)
	roots = "roots(" + rangeRev + ")"
	descendants = "descendants(" + roots + ") ~ (" + rangeRev + ")"
	return
}

func (r *repository) singleRoot(ctx context.Context, roots string) (string, error) {
	ids, err := r.revisions(ctx, roots)
	if err != nil {
		return "", err
	}
	if len(ids) != 1 {
		return "", refusal("ownership", "nonlinear", "owned stack does not have one root", "select a single linear owned stack")
	}
	return ids[0], nil
}

func (r *repository) checkLinearOwnership(ctx context.Context, line []Change, atRetarget bool) error {
	return r.checkStackOwnership(ctx, line, atRetarget, false)
}

func (r *repository) checkStackOwnership(ctx context.Context, line []Change, atRetarget, reviewOnly bool) error {
	_, roots, _ := stackRevs(line)
	merges, err := r.revisions(ctx, "merges() & "+quoteRev(line[0].Base)+".."+quoteRev(line[len(line)-1].Tip))
	if r.profile.Posture == forkTracking && r.profile.Mirror != "" {
		merges, err = r.revisions(ctx, "merges() & ("+quoteRev(line[0].Base)+".."+quoteRev(line[len(line)-1].Tip)+") ~ ::"+quoteRev(r.profile.Mirror+"@origin"))
	}
	if err != nil {
		return err
	}
	if len(merges) > 0 {
		allowed, err := r.allowedMerge(ctx, line, merges)
		if err != nil {
			return err
		}
		if !allowed {
			if _, err := r.singleRoot(ctx, roots); err != nil {
				return err
			}
			return refusal("ownership", "nonlinear", "owned stack contains a merge", "select a single linear owned stack")
		}
		if reviewOnly {
			return nil
		}
		return r.checkDescendants(ctx, "descendants("+quoteRev(line[0].Tip)+") ~ "+quoteRev(line[0].Tip), nil)
	}
	if reviewOnly {
		return nil
	}
	allowed, _, _, _, _, err := r.checkRewrite(ctx, line, atRetarget, nil) //nolint:dogsled // Only the allowed bookmark set matters for the ownership check here.
	if err != nil {
		return err
	}
	bookmarks, err := r.localBookmarks(ctx, roots+"::")
	if err != nil {
		return err
	}
	for _, bookmark := range bookmarks {
		if !allowed[bookmark] {
			return refusal("ownership", "outside-line", "rebasing would rewrite a bookmark outside the selected line: "+bookmark, "separate the selected line before submitting")
		}
	}
	return nil
}
