package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/RigelBuild/nebula/tools/go-lib/prgates"
	"github.com/samber/oops"
)

type submitOptions struct {
	bookmark, title, bodyFile, issue, ack, skip, at, hold string
	issues                                                []string
	sections                                              []namedSection
	authored, template                                    string
	skippedTargets                                        []SkippedTarget
	dryRun, retarget, review, skipGiven, holdGiven, draft bool
}
type preparedChange struct {
	change     Change
	candidate  candidate
	pr         int
	before     Metadata
	state      GateState
	baseline   Published
	exists     bool
	retarget   bool
	beforePush func()
	labelErr   error
}

//nolint:cyclop // submit is one ordered pipeline; each preflight gate feeds the next, then the push.
func (r *repository) submit(ctx context.Context, opt submitOptions, engine JJVine, log io.Writer) (err error) {
	r.landedParents = nil
	if err := validateSubmitOptions(opt); err != nil {
		return err
	}
	opt.skip = strings.TrimSpace(opt.skip)
	opt.hold = strings.TrimSpace(opt.hold)
	if err := r.clean(ctx); err != nil {
		return err
	}
	if opt.skipRequested() && !validSkipReason(r.keys, opt.skip) {
		return refusal("metadata", "skip-reason", "--skip-checks takes a one-line reason of up to 200 characters, main-red:<pr-or-issue>, or p0:"+r.keys.prefix+"-<n>", "supply a one-line reason, or run checks normally")
	}
	target, adoptionTip, err := r.submitTarget(ctx, opt)
	if err != nil {
		return err
	}
	issues, err := r.submitIssues(target, opt)
	if err != nil {
		return err
	}
	template, err := r.template(ctx)
	if err != nil {
		return err
	}
	authored, err := metadataBody(template, opt.bodyFile, opt.sections)
	if err != nil {
		return err
	}
	fullTitle, err := r.keys.FullTitle(opt.title, issues)
	if err != nil {
		return err
	}
	if !r.profile.Hooks && opt.skip == "" {
		opt.skip = r.profile.NoHooksReason
		if _, err := fmt.Fprintln(r.log, "skip-checks: "+opt.skip); err != nil {
			return toolFailure("hooks", err)
		}
	}
	body, err := r.composeAuthoredBody(template, authored, opt.skip, issues)
	if err != nil {
		return err
	}
	if result := prgates.MetadataBounds(prgates.BoundsFacts{Title: fullTitle, Body: body, IssuePrefix: r.keys.prefix}); !result.Pass {
		return refusal("metadata", result.Gate, result.Message, "edit --title or --body-file to meet the bounds")
	}
	opt.authored, opt.template = authored, template
	plan, err := r.prepareRetarget(ctx, target, opt.at)
	if err != nil {
		return err
	}
	reviewPlan, top, err := r.planSubmit(ctx, target, adoptionTip, opt)
	if err != nil {
		return err
	}
	if err := r.preflightLocalHooks(ctx, target, opt, plan, reviewPlan, top, adoptionTip); err != nil {
		return err
	}
	restoreArmed := !opt.dryRun
	defer func() {
		if restoreArmed {
			err = r.restoreRetarget(ctx, plan, err)
		}
	}()
	if err := r.armRetarget(ctx, plan, opt, log); err != nil {
		return err
	}
	prepared, err := r.preflightSubmit(ctx, target, opt, plan, adoptionTip, issues, fullTitle, body, reviewPlan, top)
	if err != nil {
		return err
	}
	if opt.dryRun {
		return r.submitDryRun(target, adoptionTip, prepared, log)
	}
	if adoptionTip != "" {
		if plan.bookmark != "" {
			adoptionTip = plan.newTip
		}
		if err := r.applyOwnedAdoption(ctx, target, adoptionTip); err != nil {
			return err
		}
	}
	if target == "" {
		target, err = r.newChange(ctx, newOptions{issue: opt.issue, issues: opt.issues, title: opt.title, at: "@-"})
		if err != nil {
			return err
		}
	}
	r.submittedBookmark = target
	line, err := r.prepareSubmitLine(ctx, target, top, opt, reviewPlan)
	if err != nil {
		return err
	}
	if err := updatePreparedLine(prepared, line); err != nil {
		return err
	}
	for _, change := range line {
		if err := r.checkEngineSelection(ctx, change, false); err != nil {
			return err
		}
	}
	if adapter, ok := engine.(vine); ok {
		adapter.line = line
		engine = adapter
	}
	store := fileBaseline{root: r.stateDir, keys: r.keys}
	for i := range prepared {
		prepared[i].candidate.path, err = store.candidate(prepared[i].change.Bookmark, prepared[i].candidate.body)
		if err != nil {
			return err
		}
	}
	return r.pushPrepared(ctx, prepared, opt, engine, log, func() {
		restoreArmed = false
	})
}

// armRetarget moves the --at bookmark for a real submit, then points the forge
// service at the local state store. A dry run moves nothing.
func (r *repository) armRetarget(ctx context.Context, plan retargetPlan, opt submitOptions, log io.Writer) error {
	if !opt.dryRun {
		if err := r.moveOptionalRetarget(ctx, plan, log); err != nil {
			return err
		}
	}
	s, err := service(ctx)
	if err != nil {
		return err
	}
	s.store = fileBaseline{root: r.stateDir, keys: r.keys}
	return nil
}

func validateSubmitOptions(opt submitOptions) error {
	if opt.skipRequested() && len(opt.skippedTargets) != 0 {
		return &failure{stage: usageStage, gate: "skip-flags", reason: "--skip-checks and --skip-target are exclusive", code: 2}
	}
	if opt.at != "" && (opt.retarget || opt.bookmark == "") {
		return &failure{stage: usageStage, gate: "at-flags", reason: "--at requires a bookmark and cannot be combined with --retarget", code: 2}
	}
	if opt.skipRequested() && !validSkipReasonShape(strings.TrimSpace(opt.skip)) {
		return refusal("metadata", "skip-reason", "--skip-checks takes a one-line reason of up to 200 characters, main-red:<pr-or-issue>, or p0:<PREFIX>-<n>", "supply a one-line reason, or run checks normally")
	}
	if (opt.holdGiven || opt.hold != "") && !validHoldReason(strings.TrimSpace(opt.hold)) {
		return refusal("metadata", "hold-reason", "--hold takes a one-line reason of up to 200 characters", "pass a one-line reason, for example --hold \"waits on EX-123\"")
	}
	return nil
}

// skipRequested reports whether --skip-checks was given, even as an empty value.
func (o submitOptions) skipRequested() bool { return o.skipGiven || o.skip != "" }

// draftOnlyBypass reports whether the bypass must leave the PR draft.
func (o submitOptions) draftOnlyBypass() bool {
	return len(o.skippedTargets) != 0 || strings.HasPrefix(o.skip, "main-red:")
}

func (r *repository) submitTarget(ctx context.Context, opt submitOptions) (string, string, error) {
	target := opt.bookmark
	var err error
	if opt.at == "" {
		target, err = r.discoverSubmitTarget(ctx, opt)
		if err != nil {
			return "", "", err
		}
	}
	if target != "" {
		if err := r.refuseClosedLedgerPR(ctx, target); err != nil {
			return "", "", err
		}
	}
	if err := r.fetchMain(ctx); err != nil {
		return "", "", err
	}
	var operation string
	if target != "" {
		operation, err = r.adoptOwnedRemote(ctx, target)
		if err != nil {
			return "", "", err
		}
	}
	if opt.at == "" && target != "" {
		if err := r.checkWorkspaceLine(ctx, target, operation); err != nil {
			return "", operation, err
		}
	}
	return target, operation, nil
}

func (r *repository) refuseClosedLedgerPR(ctx context.Context, target string) error {
	if !r.keys.Owned(r.codename, target) {
		return nil
	}
	local, err := r.revisions(ctx, "bookmarks(exact:"+quoteRev(target)+")")
	if err != nil {
		return err
	}
	if len(local) == 0 {
		return nil
	}
	ledger := Ledger{root: r.stateDir}
	entry, found, err := ledger.Find(r.profile.Name, target)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	s, err := service(ctx)
	if err != nil {
		return err
	}
	openPR, err := s.forge.findPR(ctx, r.profile.Name, target)
	if err != nil {
		return oops.Wrapf(err, "read ledger PR state")
	}
	if openPR != 0 {
		return nil
	}
	var pr struct {
		State    string
		MergedAt *string `json:"merged_at"`
		Head     struct{ Ref string }
	}
	if err := s.forge.api(ctx, http.MethodGet, prPath(r.profile.Name, entry.PR), nil, &pr); err != nil {
		failure, ok := errors.AsType[*forgeAPIError](err)
		// findPR on this repo succeeded above, so a 404 means the ledger PR is gone, not an access failure.
		if ok && failure.status == http.StatusNotFound {
			return nil
		}
		return oops.Wrapf(err, "read ledger PR state")
	}
	if pr.State == prStateClosed && pr.Head.Ref == target {
		reason := "PR for bookmark was closed without merging: " + target
		fix := "reopen the PR instead"
		if pr.MergedAt != nil {
			reason = "PR for bookmark was merged: " + target
			fix = "restack the new commits onto main, run rjc retire " + target + ", then rjc new --at <tip>"
		}
		return refusal("gate-state", "pr-closed", reason, fix)
	}
	return nil
}

func (r *repository) pushPrepared(ctx context.Context, prepared []preparedChange, opt submitOptions, engine JJVine, log io.Writer, beforeFirstPush func()) error {
	storeHooks := localPrePush{root: r.stateDir}
	changes := make([]Change, len(prepared))
	for i := range prepared {
		changes[i] = prepared[i].change
	}
	if err := r.submitHooks(ctx, changes, opt, storeHooks, log); err != nil {
		return err
	}
	var labelErr error
	for i := range prepared {
		prepared[i].beforePush = beforeFirstPush
		err := r.pushChange(ctx, &prepared[i], opt, engine, log)
		beforeFirstPush = nil
		labelErr = errors.Join(labelErr, prepared[i].labelErr)
		if err != nil {
			return errors.Join(err, labelErr)
		}
		if r.profile.Hooks {
			if err := r.publishHooks(ctx, []Change{prepared[i].change}, storeHooks); err != nil {
				return errors.Join(err, labelErr)
			}
		}
	}
	return labelErr
}

func (r *repository) submitIssues(target string, opt submitOptions) ([]int, error) {
	if target != "" {
		return r.loadIssues(target)
	}
	return parseIssues(r.keys, opt.issues, opt.issue)
}

func (r *repository) checkWorkspaceLine(ctx context.Context, bookmark, tip string) error {
	if tip == "" {
		var err error
		tip, err = r.one(ctx, quoteRev(bookmark))
		if err != nil {
			return err
		}
	}
	top, err := r.one(ctx, "@-")
	if err != nil {
		return err
	}
	commits, err := r.revisions(ctx, quoteRev(tip)+"::"+quoteRev(top))
	if err != nil {
		return err
	}
	if len(commits) == 0 {
		return refusal("working-copy", "wrong-workspace", "target bookmark is not on this workspace's committed line", "run rjc submit from the workspace that holds the commits")
	}
	return nil
}
func (r *repository) submitHooks(ctx context.Context, line []Change, opt submitOptions, store localPrePush, log io.Writer) error {
	if opt.skip != "" {
		// checkSkip validates the reason and checks Urgent priority, and saves records without pushing. A refused
		// reason stops here, before the slower root-checks run.
		if err := r.checkSkip(ctx, line, opt.skip, store); err != nil {
			return err
		}
		if strings.HasPrefix(opt.skip, "p0:") {
			return r.runP0RootChecks(ctx, line, log)
		}
		return nil
	}
	if err := r.refuseNonFastLocalHooks(ctx, line); err != nil {
		return err
	}
	if len(opt.skippedTargets) != 0 {
		if err := r.checkSkipTargets(ctx, line, opt.skippedTargets, store); err != nil {
			return err
		}
	}
	return r.runHooks(ctx, line, opt.skippedTargets, r.hookRunner(), store)
}

func (r *repository) advanceTarget(ctx context.Context, bookmark, tip string) (string, error) {
	if tip == "" {
		var err error
		tip, err = r.one(ctx, quoteRev(bookmark))
		if err != nil {
			return "", err
		}
	}
	top, err := r.one(ctx, "@-")
	if err != nil {
		return "", err
	}
	if tip == top {
		return "", nil
	}
	if _, exists, err := (fileBaseline{root: r.stateDir, keys: r.keys}).LoadUpstream(bookmark); err != nil {
		return "", err
	} else if exists {
		return "", refusal("working-copy", "nonlinear", "cannot stack work on an upstream-sync merge", "submit the upstream merge without stacked work")
	}
	path := quoteRev(tip) + "::" + quoteRev(top)
	commits, err := r.revisions(ctx, path)
	if err != nil {
		return "", err
	}
	if len(commits) == 0 {
		return "", refusal("working-copy", "wrong-workspace", "target bookmark is not on this workspace's committed line", "run rjc submit from the workspace that holds the commits")
	}
	bookmarks, err := r.localBookmarks(ctx, quoteRev(tip)+".."+quoteRev(top))
	if err != nil {
		return "", err
	}
	if len(bookmarks) > 0 {
		return "", nil
	}
	merges, err := r.revisions(ctx, "merges() & "+quoteRev(tip)+"::"+quoteRev(top))
	if err != nil {
		return "", err
	}
	if len(merges) > 0 {
		return "", refusal("working-copy", "nonlinear", "unbookmarked work includes a merge", "select a single linear bookmark line")
	}
	return top, nil
}

func (r *repository) advance(ctx context.Context, bookmark, top string) error {
	if top == "" {
		return nil
	}
	_, err := r.jj(ctx, "bookmark", "move", bookmark, "--to", top)
	return err
}

func (r *repository) describeSubmit(opt submitOptions) string {
	mode := "submitted"
	if opt.dryRun {
		mode = "prepared; no push or forge write"
	}
	return fmt.Sprintf("%s: %s; local gates: %s", strings.TrimSpace(opt.bookmark), mode, strings.Join(r.profile.LocalGates, ", "))
}
