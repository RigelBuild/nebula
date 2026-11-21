package main

import "context"

func (r *repository) planSubmit(ctx context.Context, target, selectionTip string, opt submitOptions) (*reviewFixPlan, string, error) {
	if target == "" || opt.at != "" {
		return nil, "", nil
	}
	top, err := r.advanceTarget(ctx, target, selectionTip)
	if err != nil {
		return nil, "", err
	}
	plan, err := r.planReviewFix(ctx, target, top, selectionTip)
	return plan, top, err
}

func (r *repository) submitHookLine(ctx context.Context, target, top, selectionTip string, plan *reviewFixPlan) ([]Change, error) {
	if plan == nil {
		return r.line(ctx, target, lineOptions{topTip: top, selectionTip: selectionTip, mode: linePreflight})
	}
	line := append([]Change(nil), plan.line...)
	for i := range line {
		if line[i].Bookmark == plan.parent {
			line[i].Tip = plan.fix
		}
	}
	return line, nil
}

func (r *repository) submitPreflightLine(ctx context.Context, target, top, selectionTip string, opt submitOptions, plan *reviewFixPlan) ([]Change, error) {
	var line []Change
	var err error
	if plan != nil {
		line = plan.line
	} else {
		line, err = r.line(ctx, target, lineOptions{topTip: top, selectionTip: selectionTip, mode: linePreflight, atRetarget: opt.at != ""})
	}
	if err != nil {
		return nil, err
	}
	if err := r.refuseDivergentLine(ctx, line); err != nil {
		return nil, err
	}
	for _, change := range line {
		explicit := "(" + quoteRev(change.Tip) + " | bookmarks(exact:" + quoteRev(change.Bookmark) + "))"
		if change.Bookmark == target && selectionTip != "" {
			explicit = quoteRev(change.Tip)
		}
		if err := r.checkEngineSelectionAt(ctx, change, change.Base, explicit); err != nil {
			return nil, err
		}
		if plan != nil && change.Bookmark == plan.parent {
			if err := r.checkEngineSelectionAt(ctx, change, change.Base, "("+quoteRev(plan.fix)+" | bookmarks(exact:"+quoteRev(change.Bookmark)+"))"); err != nil {
				return nil, err
			}
		}
	}
	return line, nil
}

func setReviewFixTip(prepared []preparedChange, plan *reviewFixPlan) {
	if plan == nil {
		return
	}
	for i := range prepared {
		if prepared[i].change.Bookmark == plan.parent {
			prepared[i].change.Tip = plan.fix
		}
	}
}

func publishedSubmitLine(prepared []preparedChange, target string, plan *reviewFixPlan) []preparedChange {
	if plan == nil {
		return prepared
	}
	result := prepared[:0]
	for _, item := range prepared {
		if item.change.Bookmark == target || item.pr != 0 {
			result = append(result, item)
		}
	}
	return result
}

func updatePreparedLine(prepared []preparedChange, line []Change) error {
	for i := range prepared {
		found := false
		for _, change := range line {
			if prepared[i].change.Bookmark == change.Bookmark {
				prepared[i].change = change
				found = true
				break
			}
		}
		if !found {
			return refusal("ownership", "changed-line", "prepared bookmark is missing from the rewritten line", "rerun rjc submit")
		}
	}
	return nil
}

func (r *repository) prepareSubmitLine(ctx context.Context, target, top string, opt submitOptions, plan *reviewFixPlan) ([]Change, error) {
	if plan != nil {
		return r.applyReviewFix(ctx, plan)
	}
	if opt.at == "" {
		if err := r.advance(ctx, target, top); err != nil {
			return nil, err
		}
	}
	line, err := r.line(ctx, target, lineOptions{atRetarget: opt.at != "", mode: linePreflight})
	if err != nil {
		return nil, err
	}
	return r.rebaseLine(ctx, line, opt.at != "")
}
