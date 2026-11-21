package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/samber/oops"
)

// pushedHeadBackoff spaces the PR head re-reads after a push. Its waits total 30 s.
var pushedHeadBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second}

//nolint:cyclop // One push transaction: retarget, push, confirm, and reconcile each read the state the earlier step set.
func (r *repository) pushChange(ctx context.Context, item *preparedChange, opt submitOptions, engine JJVine, log io.Writer) (resultErr error) {
	s, err := service(ctx)
	if err != nil {
		return err
	}
	if err := r.guardBeforePush(ctx, item); err != nil {
		return err
	}
	if !r.keys.Owned(r.codename, item.change.Bookmark) {
		return refusal("jj-vine", "ownership", "bookmark is not owned by the caller", "use the caller's codename bookmark")
	}
	if err := r.checkEngineSelection(ctx, item.change, true); err != nil {
		return err
	}
	patched := item.retarget && item.before.BaseRef != r.profile.Trunk
	baseChanged, headMayHaveMoved := false, false
	// holdDraft opens a new PR as draft so it is never Ready before its Hold posts.
	holdDraft := item.pr == 0 && opt.hold != "" && !opt.draft && !opt.draftOnlyBypass()
	holdPR := 0
	// Registered first so it runs last: the run's own comments come first and the Hold is the newest word on the PR.
	defer func() {
		if opt.hold != "" && holdPR != 0 {
			resultErr = errors.Join(resultErr, r.holdThenReady(ctx, s.forge, holdPR, opt.hold, holdDraft))
		}
	}()
	defer func() {
		if item.retarget && (baseChanged || headMayHaveMoved) {
			resultErr = errors.Join(resultErr, requestRetargetReview(ctx, r.profile.Reviewer, r.profile.Name, item.pr, r.profile.Trunk))
		}
	}()
	if patched {
		if _, err := retargetPR(ctx, r.profile.Name, item.pr, item.before.BaseRef, r.profile.Trunk, item.before.HeadSHA, &baseChanged); err != nil {
			return err
		}
	}
	if item.beforePush != nil {
		item.beforePush()
	}
	if opt.draft && item.pr != 0 {
		if _, err := fmt.Fprintf(log, "PR #%d exists; --draft applies only at creation; set draft: true via GitHub MCP, or run gh pr ready --undo %d -R %s if MCP is unavailable\n", item.pr, item.pr, r.profile.Name); err != nil {
			return toolFailure(submitCommandName, err)
		}
	}
	pushErr := engine.Submit(ctx, item.change.Bookmark, item.candidate.path, true, item.pr == 0 && (opt.draft || opt.draftOnlyBypass() || holdDraft), log)
	if pushErr != nil {
		return r.pushFailure(ctx, item, patched, true, pushErr, &headMayHaveMoved, &baseChanged)
	}
	headMayHaveMoved = true
	if item.pr != 0 {
		if err := (Ledger{root: r.stateDir}).recordPush(r.profile.Name, item.change.Bookmark, item.pr, r.codename, item.change.Tip); err != nil {
			return err
		}
	}
	pr, lookupErr := s.forge.findPR(ctx, r.profile.Name, item.change.Bookmark)
	if lookupErr != nil {
		return r.pushFailure(ctx, item, patched, false, lookupErr, &headMayHaveMoved, &baseChanged)
	}
	if pr == 0 {
		return r.pushFailure(ctx, item, patched, false, oops.Errorf("jj-vine did not create or preserve the bookmark's PR"), &headMayHaveMoved, &baseChanged)
	}
	holdPR = pr
	after, readErr := r.awaitPushedHead(ctx, s.forge, pr, item.change)
	if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
		stage, fix := "jj-vine", "inspect the PR and rerun rjc submit"
		if item.retarget {
			stage, fix = retargetStage, fix+" --retarget"
		}
		if patched {
			fix = retargetBaseFix
		}
		return &failure{stage: stage, gate: toolFailureGate, reason: fmt.Sprintf("interrupted while confirming push of PR %d", pr), fix: fix, code: 3, cause: readErr}
	}
	if readErr != nil {
		return r.pushFailure(ctx, item, patched, false, readErr, &headMayHaveMoved, &baseChanged)
	}
	var movedErr error
	if !item.retarget {
		movedErr = ReRequestIfMoved(ctx, r.profile.Reviewer, r.profile.Name, pr, item.state.Coverage != Uncovered, item.before.HeadSHA, after.HeadSHA)
	}
	if item.retarget && (after.HeadSHA != item.change.Tip || after.HeadRef != item.change.Bookmark) {
		return refusal("retarget", "head-base-changed", fmt.Sprintf("PR head changed before retarget (observed %s on %s)", after.HeadSHA, after.HeadRef), "rerun rjc submit with a fresh snapshot")
	}
	if after.HeadSHA != item.change.Tip || after.HeadRef != item.change.Bookmark {
		return errors.Join(movedErr, refusal("jj-vine", "unconfirmed-push", fmt.Sprintf("remote head does not match the submitted bookmark (observed %s on %s)", after.HeadSHA, after.HeadRef), "inspect jj-vine output before rerunning"))
	}
	if after.BaseRef != strings.TrimSuffix(item.change.Base, "@origin") {
		return errors.Join(movedErr, refusal("jj-vine", "unconfirmed-base", "remote PR base does not match the validated parent boundary", "resolve the PR base before rerunning"))
	}
	if err := (Ledger{root: r.stateDir}).recordPush(r.profile.Name, item.change.Bookmark, pr, r.codename, after.HeadSHA); err != nil {
		return err
	}
	if r.landedParents[item.change.Bookmark] != "" && item.change.Base == r.profile.TrunkRemote() && after.BaseRef == r.profile.Trunk {
		if err := r.clearParent(item.change.Bookmark); err != nil {
			return errors.Join(movedErr, err)
		}
	}
	return r.reconcileConfirmedPush(ctx, item, opt, confirmedPush{pr: pr, metadata: after, movedErr: movedErr})
}

// awaitPushedHead re-reads the PR until its head and ref match the pushed change.
// GitHub can lag a push, so it backs off up to 30 s, then returns the last read.
func (r *repository) awaitPushedHead(ctx context.Context, forge *forgeClient, pr int, change Change) (Metadata, error) {
	wait := r.pushedHeadWait
	if wait == nil {
		wait = waitFor
	}
	for attempt := 0; ; attempt++ {
		after, err := forge.metadata(ctx, r.profile.Name, pr)
		if err != nil {
			return Metadata{}, err
		}
		if (after.HeadSHA == change.Tip && after.HeadRef == change.Bookmark) || attempt == len(pushedHeadBackoff) {
			return after, nil
		}
		if err := wait(ctx, pushedHeadBackoff[attempt]); err != nil {
			return Metadata{}, err
		}
	}
}

type confirmedPush struct {
	pr       int
	metadata Metadata
	movedErr error
}

func (r *repository) reconcileConfirmedPush(ctx context.Context, item *preparedChange, opt submitOptions, pushed confirmedPush) error {
	if err := r.refreshParentBoundary(ctx, item.change.Bookmark, item.change.Tip, lineRecord); err != nil {
		return errors.Join(pushed.movedErr, err)
	}
	baseline := item.baseline
	if item.pr == 0 {
		baseline = Published{Title: item.candidate.title, Authored: item.candidate.body}
	}
	want := pushed.metadata
	want.Title = item.candidate.title
	want.Body = item.candidate.body
	s, err := service(ctx)
	if err != nil {
		return err
	}
	s.bookmark = item.change.Bookmark
	reconcileErr := ReconcileMetadata(ctx, r.profile.Name, pushed.pr, baseline, opt.ack, want)
	if reconcileErr != nil {
		return errors.Join(pushed.movedErr, reconcileErr)
	}
	if item.change.Bookmark == r.submittedBookmark {
		_, synced, err := r.recordedUpstreamMerge(ctx, item.change.Bookmark, item.change.Tip)
		if err != nil {
			return errors.Join(pushed.movedErr, upstreamSyncLabelFailure(pushed.pr, err))
		}
		if synced {
			if err := requestUpstreamSyncLabel(ctx, r.profile.Name, pushed.pr, pushed.metadata); err != nil {
				return errors.Join(pushed.movedErr, err)
			}
		}
	}
	// A design-label failure is held and returned after the stack pushes, so one failed label never stops it.
	if r.profile.DesignLabel {
		item.labelErr = r.requestDesignLabel(ctx, item.change, pushed.pr, pushed.metadata)
	}
	if !opt.review || item.change.Bookmark != r.submittedBookmark {
		return pushed.movedErr
	}
	if err := requestAgentReview(ctx, r.profile.Name, pushed.pr, pushed.metadata); err != nil {
		return agentReviewFailure(pushed.pr, errors.Join(pushed.movedErr, err))
	}
	return pushed.movedErr
}

func (r *repository) guardBeforePush(ctx context.Context, item *preparedChange) error {
	if item.pr == 0 {
		return nil
	}
	if item.retarget {
		s, err := service(ctx)
		if err != nil {
			return err
		}
		fresh, err := s.forge.metadata(ctx, r.profile.Name, item.pr)
		if err != nil {
			return err
		}
		if fresh.BaseRef != item.before.BaseRef || fresh.HeadSHA != item.before.HeadSHA {
			return refusal("retarget", "head-base-changed", "PR head or base changed since preflight", "rerun rjc submit with a fresh snapshot")
		}
	}
	state, err := ReadGateState(ctx, r.profile.Name, item.pr)
	if err != nil {
		return err
	}
	if state.Coverage != Uncovered {
		item.state.Coverage = state.Coverage
	}
	return nil
}

func (r *repository) pushFailure(ctx context.Context, item *preparedChange, patched, pushFailed bool, cause error, headMoved, baseChanged *bool) error {
	fix := "inspect the PR and rerun rjc submit"
	stage := "jj-vine"
	if item.retarget {
		fix += " --retarget"
		stage = retargetStage
	}
	if item.pr == 0 {
		return cause
	}
	_, fetchErr := r.jj(ctx, "git", "fetch", "--remote", originRemote)
	var head string
	if fetchErr == nil {
		head, fetchErr = r.one(ctx, quoteRev(item.change.Bookmark+"@origin"))
	}
	originKnown := fetchErr == nil && head != ""
	if !originKnown {
		s, err := service(ctx)
		if err == nil {
			var metadata Metadata
			metadata, err = s.forge.metadata(ctx, r.profile.Name, item.pr)
			head = metadata.HeadSHA
		}
		if err != nil || head == "" {
			*headMoved = true
			return &failure{stage: stage, gate: toolFailureGate, reason: fmt.Sprintf("cannot confirm head of PR %d after push", item.pr), fix: fix, code: 3, cause: errors.Join(cause, fetchErr, err)}
		}
	}
	if head != item.before.HeadSHA {
		*headMoved = true
	}
	var reviewErr error
	if !item.retarget {
		reviewErr = ReRequestIfMoved(ctx, r.profile.Reviewer, r.profile.Name, item.pr, item.state.Coverage != Uncovered, item.before.HeadSHA, head)
	}
	if patched && pushFailed && originKnown && head == item.before.HeadSHA {
		if err := r.restoreRetargetBase(ctx, item.pr, item.before.BaseRef); err != nil {
			return &failure{stage: stage, gate: toolFailureGate, reason: fmt.Sprintf("could not restore base of PR %d; base remains on trunk: %v", item.pr, err), fix: fix, code: 3, cause: errors.Join(cause, reviewErr, err)}
		}
		// A confirmed restore reverses the successful base patch.
		*baseChanged = false
	} else if patched {
		fix = retargetBaseFix
		return &failure{stage: stage, gate: toolFailureGate, reason: fmt.Sprintf("push or readback failed for PR %d; base remains on trunk", item.pr), fix: fix, code: 3, cause: errors.Join(cause, reviewErr)}
	}
	return errors.Join(cause, reviewErr)
}
