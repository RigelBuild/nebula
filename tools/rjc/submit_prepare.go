package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

func (r *repository) discoverSubmitTarget(ctx context.Context, opt submitOptions) (string, error) {
	target := opt.bookmark
	if target == "" {
		bookmarks, err := r.localBookmarks(ctx, quoteRev(r.profile.TrunkRemote())+"..@-")
		if err != nil {
			return "", err
		}
		for _, bookmark := range bookmarks {
			if r.keys.Owned(r.codename, bookmark) {
				target = bookmark
			}
		}
		if target == "" {
			if opt.issue == "" && len(opt.issues) == 0 {
				return "", refusal("working-copy", "missing-issue", "no owned bookmark on @-'s line", "rjc submit --issue <PREFIX>-<n> --title <title> --body-file <path>")
			}
			return "", nil
		}
	}
	if !r.keys.Owned(r.codename, target) {
		return "", refusal("ownership", "foreign-bookmark", "cannot submit a bookmark owned by another caller", "use a bookmark prefixed with "+r.codename+"/")
	}
	values := opt.issues
	if len(values) == 0 && opt.issue != "" {
		values = []string{opt.issue}
	}
	if len(values) > 0 {
		stored, err := r.loadIssues(target)
		if err != nil {
			return "", err
		}
		issues, err := parseIssues(r.keys, values, "")
		if err != nil {
			return "", err
		}
		if !slices.Equal(issues, stored) {
			return "", refusal("gate-state", "issue-mismatch", "--issue differs from the bookmark's stored issues", "omit --issue or provide the stored issues in order")
		}
	}
	return target, nil
}

func (r *repository) prepareLine(ctx context.Context, line []Change, target string, opt submitOptions) ([]preparedChange, error) {
	s, err := service(ctx)
	if err != nil {
		return nil, err
	}
	prepared := make([]preparedChange, 0, len(line))
	for _, change := range line {
		pr, err := s.forge.findPR(ctx, r.profile.Name, change.Bookmark)
		if err != nil {
			return nil, err
		}
		item := preparedChange{change: change, pr: pr}
		if pr != 0 {
			if change.Bookmark == target && (opt.issue != "" || len(opt.issues) > 0) {
				return nil, refusal("gate-state", "issue-on-existing-pr", "--issue is not allowed on an existing PR", "omit --issue; the bookmark already carries its issue")
			}
			item.retarget = opt.retarget && change.Bookmark == target
			if err := r.readExisting(ctx, &item, opt.ack); err != nil {
				return nil, err
			}
		}
		if opt.retarget && change.Bookmark == target && pr == 0 {
			return nil, refusal("gate-state", "base", "retarget requires an existing open PR", "retarget an existing bookmark with an open PR")
		}
		prepared = append(prepared, item)
	}
	return prepared, nil
}

func (r *repository) readExisting(ctx context.Context, item *preparedChange, ack string) error {
	s, err := service(ctx)
	if err != nil {
		return err
	}
	item.before, err = s.forge.metadata(ctx, r.profile.Name, item.pr)
	if err != nil {
		return err
	}
	tracked, err := r.revisions(ctx, quoteRev(item.change.Bookmark+"@origin"))
	if err != nil {
		return err
	}
	if item.before.HeadRef != item.change.Bookmark || len(tracked) != 1 || tracked[0] != item.before.HeadSHA {
		return refusal("gate-state", "ownership", "PR head differs from the freshly fetched owned bookmark", "resolve the remote bookmark and PR identity before submitting")
	}
	expectedBase := strings.TrimSuffix(item.change.Base, "@origin")
	if item.retarget {
		if item.before.BaseRef == r.profile.Trunk && item.before.HeadSHA == item.change.Tip {
			return refusal("gate-state", "base", "PR already targets trunk at the local head", "omit --retarget")
		}
		eligible := r.profile.Posture != forkNone && item.change.Base == r.profile.TrunkRemote()
		eligible = eligible && (item.before.BaseRef == r.profile.Trunk || item.before.BaseRef == r.profile.Mirror && r.profile.Posture == forkTracking || r.landedParents[item.change.Bookmark] == item.before.BaseRef && item.before.BaseRef != "")
		if !eligible {
			return refusal("gate-state", "base", "PR base is not an eligible fork mirror or landed parent", "retarget only an eligible PR to the trunk")
		}
	} else if item.before.BaseRef != expectedBase {
		if r.profile.Posture == forkTracking && item.before.BaseRef == r.profile.Mirror {
			return refusal("gate-state", "fork-base-mismatch", "tracking fork PR targets the mirror", "rjc sync, then rjc submit --retarget")
		}
		if item.change.Base != r.profile.TrunkRemote() || r.landedParents[item.change.Bookmark] != item.before.BaseRef {
			return refusal("gate-state", "base", "PR base differs from the selected bookmark boundary", "resolve the PR base and bookmark line before submitting")
		}
	}
	item.state, err = ReadGateState(ctx, r.profile.Name, item.pr)
	if err != nil {
		return err
	}
	item.baseline, item.exists, err = s.store.Load(item.change.Bookmark)
	if err != nil {
		return err
	}
	_, authored, err := splitBody(item.before.Body)
	if err != nil {
		return err
	}
	return guardAuthored(item.baseline, item.exists, authored, ack)
}

func (r *repository) rebaseLine(ctx context.Context, line []Change, atRetarget bool) ([]Change, error) {
	merges, err := r.revisions(ctx, "merges() & "+quoteRev(line[0].Base)+".."+quoteRev(line[len(line)-1].Tip))
	if r.profile.Posture == forkTracking {
		merges, err = r.revisions(ctx, "merges() & ("+quoteRev(line[0].Base)+".."+quoteRev(line[len(line)-1].Tip)+") ~ ::"+quoteRev(r.profile.Mirror+"@origin"))
	}
	if err != nil {
		return nil, err
	}
	if len(merges) != 0 {
		allowed, err := r.allowedMerge(ctx, line, merges)
		if err != nil {
			return nil, err
		}
		if !allowed {
			return nil, refusal("rebase", "nonlinear", "owned stack contains a merge", "select a single linear owned stack")
		}
		return line, nil
	}
	excused, err := r.excusedOwnHeads(ctx, line)
	if err != nil {
		return nil, err
	}
	if err := r.rebaseOwned(ctx, line, atRetarget, excused); err != nil {
		return nil, err
	}
	return r.line(ctx, line[len(line)-1].Bookmark, lineOptions{atRetarget: atRetarget, mode: linePreflight})
}

func (r *repository) checkForeignPublished(ctx context.Context, line []Change, allowed map[string]bool) error {
	foreign, err := r.foreignPublishedCommits(ctx, line, allowed)
	if err != nil {
		return err
	}
	if len(foreign) > 0 {
		return refusal("ownership", "foreign-published", "source includes published commits outside the owned bookmarks", "select a base after the published work")
	}
	return nil
}

func (r *repository) foreignPublishedCommits(ctx context.Context, line []Change, allowed map[string]bool) ([]string, error) {
	rangeRev, _, descendants := stackRevs(line)
	var revset strings.Builder
	revset.WriteString("((" + rangeRev + ") | (" + descendants + ")) & ::remote_bookmarks(remote=exact:\"origin\")")
	for name := range allowed {
		revset.WriteString(" ~ ::remote_bookmarks(exact:" + quoteRev(name) + ", remote=exact:\"origin\")")
	}
	return r.revisions(ctx, revset.String())
}

func (r *repository) preparePayloads(ctx context.Context, prepared []preparedChange, target string, opt submitOptions) error {
	for i := range prepared {
		item := &prepared[i]
		if item.pr != 0 && !item.before.Draft && opt.draftOnlyBypass() {
			return refusal("metadata", "skip-on-ready-pr", "main-red skips and --skip-target must leave the PR draft", fmt.Sprintf("set draft: true via GitHub MCP, or run gh pr ready --undo %d -R %s if MCP is unavailable", item.pr, r.profile.Name))
		}
		var err error
		switch {
		case item.change.Bookmark == target:
			item.candidate, err = r.prepareResolvedCandidate(ctx, target, item.change.Tip, opt.title, opt.authored, opt.template, opt.skip)
		case item.pr == 0:
			item.candidate, err = r.loadCandidate(item.change.Bookmark)
		default:
			_, body, splitErr := splitBody(item.before.Body)
			if splitErr != nil {
				return splitErr
			}
			item.candidate = candidate{title: item.before.Title, body: body}
		}
		if err != nil {
			return err
		}
		if item.change.Bookmark != target {
			body, err := r.composeCandidateWithTemplate(item.change.Bookmark, item.candidate.body, opt.skip, opt.template)
			if err != nil {
				return err
			}
			item.candidate.body = body
		}
		issues, err := r.loadIssues(item.change.Bookmark)
		if err != nil {
			return err
		}
		if err := r.checkCandidateWithInputs(ctx, item.change, item.candidate, opt.skip, opt.template, issues); err != nil {
			return err
		}
	}
	return nil
}

// checkRewrite refuses unsafe rewrites and returns allowed bookmarks, the rebase
// root, stale published copies with their visible before-set, and the excused
// set widened to the history under each excused head.
func (r *repository) checkRewrite(ctx context.Context, line []Change, atRetarget bool, excused map[string]bool) (map[string]bool, string, []string, []string, map[string]bool, error) {
	_, roots, descendants := stackRevs(line)
	allowed, err := r.allowedBookmarks(ctx, line, atRetarget)
	if err != nil {
		return nil, "", nil, nil, nil, err
	}
	root, err := r.singleRoot(ctx, roots)
	if err != nil {
		return nil, "", nil, nil, nil, err
	}
	if err := r.checkForeignPublished(ctx, line, allowed); err != nil {
		return nil, "", nil, nil, nil, err
	}
	stale, staleChanges, err := r.staleRewriteCopies(ctx, line, root, allowed)
	if err != nil {
		return nil, "", nil, nil, nil, err
	}
	widened, err := r.checkDescendantsExcept(ctx, descendants, allowed, atRetarget, "", excused)
	if err != nil {
		return nil, "", nil, nil, nil, err
	}
	return allowed, root, stale, staleChanges, widened, nil
}

// allowedBookmarks returns the bookmarks the rewrite may move: the selected
// line, and on submit --at a local owned chain directly above the new tip.
func (r *repository) allowedBookmarks(ctx context.Context, line []Change, atRetarget bool) (map[string]bool, error) {
	allowed := make(map[string]bool, len(line))
	for _, change := range line {
		allowed[change.Bookmark] = true
	}
	if !atRetarget {
		return allowed, nil
	}
	tip := quoteRev(line[len(line)-1].Tip)
	above, err := r.localBookmarks(ctx, "("+tip+"::) ~ "+tip)
	if err != nil {
		return nil, err
	}
	var heads []string
	for _, name := range above {
		if !r.keys.Owned(r.codename, name) {
			continue
		}
		allowed[name] = true
		heads = append(heads, "bookmarks(exact:"+quoteRev(name)+")")
	}
	if len(heads) == 0 {
		return allowed, nil
	}
	chain := "((" + tip + "::) ~ " + tip + ") & ::(" + strings.Join(heads, " | ") + ")"
	chainHeads, err := r.revisions(ctx, "heads("+chain+")")
	if err != nil {
		return nil, err
	}
	if len(chainHeads) != 1 {
		return nil, refusal("ownership", "outside-line", "owned bookmarks above the selected tip do not form one line", "separate the selected line before submitting")
	}
	return allowed, nil
}

func (r *repository) checkDescendants(ctx context.Context, descendants string, allowed map[string]bool) error {
	_, err := r.checkDescendantsExcept(ctx, descendants, allowed, false, "", nil)
	return err
}

// checkDescendantsExcept refuses descendants of the line that rjc may not
// rewrite. It reads excused and never writes it. It returns the excused set
// widened to the history under each excused head, so the caller can abandon
// those commits with the rebase.
func (r *repository) checkDescendantsExcept(ctx context.Context, descendants string, allowed map[string]bool, atRetarget bool, extra string, excused map[string]bool) (map[string]bool, error) {
	scope := descendants
	var work strings.Builder
	work.WriteString("(" + scope + ")")
	if extra != "" {
		work.WriteString(" ~ ::" + quoteRev(extra))
	}
	for name := range allowed {
		remote := "remote_bookmarks(exact:" + quoteRev(name) + ", remote=exact:\"origin\")"
		work.WriteString(" ~ ::" + remote)
	}
	bookmarks, err := r.stackBookmarks(ctx, scope)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, bookmark := range bookmarks {
		name := strings.TrimSuffix(bookmark, "@origin")
		if !allowed[name] {
			if atRetarget && r.keys.Owned(r.codename, name) {
				return nil, refusal("ownership", "outside-line", "rebasing would rewrite a bookmark outside the selected line: "+name, "separate the selected line before submitting")
			}
			return nil, refusal("ownership", "foreign-descendant", "rebasing would rewrite a descendant bookmark: "+name, "separate the descendant before submitting")
		}
		if !seen[name] {
			seen[name] = true
			work.WriteString(" ~ ::bookmarks(exact:" + quoteRev(name) + ") ~ ::remote_bookmarks(exact:" + quoteRev(name) + ", remote=exact:\"origin\")")
		}
	}
	commits, err := r.revisions(ctx, work.String()+" ~ @ ~ (empty() & description(exact:\"\"))")
	if err != nil {
		return nil, err
	}
	history := make(map[string]bool, len(excused))
	for commit := range excused {
		history[commit] = true
	}
	if len(commits) == 0 {
		return history, nil
	}
	recorded, err := r.recordedOwnHeads(ctx, allowed)
	if err != nil {
		return nil, err
	}
	if err := r.addOwnHistory(ctx, recorded, commits); err != nil {
		return nil, err
	}
	// The submit rebase abandons excused commits after this check, so the
	// history under each excused head joins the set. Left out, a commit under
	// an old head would be carried along by the rebase and refused as foreign
	// by the next line check.
	if err := r.addOwnHistory(ctx, history, commits); err != nil {
		return nil, err
	}
	for _, commit := range commits {
		if !recorded[commit] && !history[commit] {
			return nil, refusal("ownership", "foreign-descendant", "rebasing would rewrite descendant work", "separate the descendant before submitting")
		}
	}
	return history, nil
}

// addOwnHistory adds to heads every commit of scope that is an ancestor of a
// head and not on main. rjc pushed each recorded head with its whole history,
// so those commits are rjc's own earlier work too. Only commits of scope are
// added, so a commit outside it is never excused. A nil or empty heads map
// is left unchanged.
func (r *repository) addOwnHistory(ctx context.Context, heads map[string]bool, scope []string) error {
	if len(heads) == 0 || len(scope) == 0 {
		return nil
	}
	ids := make([]string, 0, len(heads))
	for head := range heads {
		ids = append(ids, "present("+quoteRev(head)+")")
	}
	ancestors, err := r.revisions(ctx, "::("+strings.Join(ids, " | ")+") ~ ::"+quoteRev(r.profile.TrunkRemote()))
	if err != nil {
		return err
	}
	inScope := make(map[string]bool, len(scope))
	for _, commit := range scope {
		inScope[commit] = true
	}
	for _, commit := range ancestors {
		if inScope[commit] {
			heads[commit] = true
		}
	}
	return nil
}

// recordedOwnHeads returns the heads the rjc ledger recorded as pushed for each
// allowed bookmark's open PR. The ledger is shared by every seat on the host, but
// a PR's entry is looked up by its repo, bookmark and PR number. A descendant
// equal to one of these heads is rjc's own earlier push, not foreign work.
func (r *repository) recordedOwnHeads(ctx context.Context, allowed map[string]bool) (map[string]bool, error) {
	s, err := service(ctx)
	if err != nil {
		return nil, err
	}
	heads := make(map[string]bool, len(allowed))
	ledger := Ledger{root: r.stateDir}
	for name := range allowed {
		pr, err := s.forge.findPR(ctx, r.profile.Name, name)
		if err != nil {
			return nil, err
		}
		if pr == 0 {
			continue
		}
		entry, found, err := ledger.findForPR(r.profile.Name, name, pr)
		if err != nil {
			return nil, err
		}
		if found && entry.PR == pr {
			for _, head := range append([]string{entry.LastPushedHead}, entry.PushedHeads...) {
				if head != "" {
					heads[head] = true
				}
			}
		}
	}
	return heads, nil
}

// excusedOwnHeads returns the commits a submit may excuse on the line: rjc's own
// recorded pushed heads for the line's owned bookmarks, each open on the forge.
// Only the submit rewrite path calls it.
func (r *repository) excusedOwnHeads(ctx context.Context, line []Change) (map[string]bool, error) {
	owned := make(map[string]bool, len(line))
	for _, change := range line {
		if r.keys.Owned(r.codename, change.Bookmark) {
			owned[change.Bookmark] = true
		}
	}
	return r.recordedOwnHeads(ctx, owned)
}

// refuseDivergentLine refuses a line whose change has a second visible copy
// outside descendants of the line's roots. Submit's rebase would leave that copy
// divergent and the engine would fail on it, so submit runs this before any
// mutation.
func (r *repository) refuseDivergentLine(ctx context.Context, line []Change) error {
	rangeRev, roots, _ := stackRevs(line)
	lineCopies := "divergent() & (" + rangeRev + ")"
	out, err := r.jj(ctx, "log", "--no-graph", "-r", lineCopies, "-T", `change_id ++ "\n"`)
	if err != nil || out == "" {
		return err
	}
	ids := make([]string, 0, len(line))
	for id := range strings.FieldsSeq(out) {
		ids = append(ids, "change_id("+quoteRev(id)+")")
	}
	outsideRev := "(" + strings.Join(ids, " | ") + ") ~ descendants(" + roots + ")"
	outside, err := r.describeCommits(ctx, outsideRev)
	if err != nil || outside == "" {
		return err
	}
	local, err := r.describeCommits(ctx, lineCopies)
	if err != nil {
		return err
	}
	return refusal("ownership", "divergent-line",
		"published copy outside the rebase root diverges from the local line: published "+outside+"; local "+local,
		"give the local copies new change IDs with jj metaedit --update-change-id -r <local commit>, then retry")
}

// describeCommits names each commit in rev by change, commit, and bookmarks.
func (r *repository) describeCommits(ctx context.Context, rev string) (string, error) {
	out, err := r.jj(ctx, "log", "--no-graph", "-r", rev, "-T", `change_id.short() ++ " " ++ commit_id.short() ++ " " ++ bookmarks.join(",") ++ "\n"`)
	if err != nil || out == "" {
		return "", err
	}
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	return strings.Join(lines, "; "), nil
}

// staleRewriteCopies refuses visible stale copies and returns the commits to abandon.
func (r *repository) staleRewriteCopies(ctx context.Context, line []Change, root string, allowed map[string]bool) ([]string, []string, error) {
	remotes := make([]string, 0, len(allowed))
	locals := make([]string, 0, len(allowed))
	for name := range allowed {
		remotes = append(remotes, "::remote_bookmarks(exact:"+quoteRev(name)+", remote=exact:\"origin\")")
		locals = append(locals, "::bookmarks(exact:"+quoteRev(name)+")")
	}
	scope := "(" + strings.Join(remotes, " | ") + ") & descendants(" + quoteRev(root) + ") ~ ::(" + quoteRev(line[len(line)-1].Tip) + ") ~ (" + strings.Join(locals, " | ") + ")"
	commits, err := r.revisions(ctx, scope)
	if err != nil {
		return nil, nil, err
	}
	visible, err := r.revisions(ctx, "working_copies() & descendants("+scope+")")
	if err != nil {
		return nil, nil, err
	}
	if len(visible) > 0 {
		workspaces, err := r.jj(ctx, "workspace", "list")
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, refusal("ownership", "stale-copy-visible", "workspaces are on or above stale published copies: "+strings.Join(commits, ", ")+"; workspaces: "+workspaces, "move the workspace with jj workspace update-stale or jj new <tip>, or abandon the old copy, then retry")
	}
	ids := make([]string, 0, len(commits))
	for _, commit := range commits {
		id, err := r.jj(ctx, "log", "--no-graph", "-r", quoteRev(commit), "-T", "change_id")
		if err != nil {
			return nil, nil, err
		}
		ids = append(ids, "change_id("+quoteRev(id)+")")
	}
	if len(ids) == 0 {
		return commits, ids, nil
	}
	before, err := r.revisions(ctx, strings.Join(ids, " | "))
	return before, ids, err
}

func (r *repository) abandonStaleRewrites(ctx context.Context, stale, ids []string, allowed map[string]bool) error {
	if len(stale) == 0 {
		return nil
	}
	original := make([]string, 0, len(stale))
	for _, commit := range stale {
		original = append(original, "present("+quoteRev(commit)+")")
	}
	rewrites := "(" + strings.Join(ids, " | ") + ") ~ (" + strings.Join(original, " | ") + ") ~ ::bookmarks() ~ working_copies()"
	commits, err := r.revisions(ctx, rewrites)
	if err != nil {
		return err
	}
	if len(commits) > 0 {
		if _, err := r.jj(ctx, "abandon", "--ignore-immutable", "-r", rewrites); err != nil {
			return err
		}
	}
	remotes := make([]string, 0, len(allowed))
	for name := range allowed {
		remotes = append(remotes, "remote_bookmarks(exact:"+quoteRev(name)+", remote=exact:\"origin\")")
		remotes = append(remotes, "bookmarks(exact:"+quoteRev(name)+")")
	}
	remainingRev := "divergent() & (" + strings.Join(ids, " | ") + ")"
	if len(remotes) > 0 {
		remainingRev += " ~ (" + strings.Join(remotes, " | ") + ")"
	}
	remaining, err := r.revisions(ctx, remainingRev)
	if err != nil {
		return err
	}
	if len(remaining) > 0 {
		return fmt.Errorf("stale rebase left divergent commits: %s", strings.Join(remaining, ", "))
	}
	return nil
}
