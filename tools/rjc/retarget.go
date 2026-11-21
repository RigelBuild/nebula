package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

const (
	retargetStage   = "retarget"
	retargetBaseFix = "base stays on trunk; inspect the PR and rerun rjc submit --retarget"
)

func retargetPR(ctx context.Context, repo string, pr int, oldBase, newBase, head string, patched *bool) (Metadata, error) {
	s, err := service(ctx)
	if err != nil {
		return Metadata{}, err
	}
	before, err := s.forge.metadata(ctx, repo, pr)
	if err != nil {
		return Metadata{}, err
	}
	if before.BaseRef != oldBase || before.HeadSHA != head {
		return Metadata{}, refusal("retarget", "head-base-changed", "PR head or base changed before retarget", "rerun rjc submit with a fresh snapshot")
	}
	*patched = true
	if err := s.forge.api(ctx, http.MethodPatch, prPath(repo, pr), map[string]string{"base": newBase}, nil); err != nil {
		return Metadata{}, retargetPatchFailure(pr, err)
	}
	after, err := s.forge.metadata(ctx, repo, pr)
	if err != nil {
		return Metadata{}, retargetPatchFailure(pr, err)
	}
	if after.BaseRef != newBase || after.HeadSHA != head {
		cause := refusal("retarget", "head-base-changed", "PR head or base changed during retarget", "inspect the PR before retrying")
		return Metadata{}, retargetPatchFailure(pr, cause)
	}
	return after, nil
}

func retargetPatchFailure(pr int, cause error) error {
	return &failure{
		stage:  retargetStage,
		gate:   toolFailureGate,
		reason: fmt.Sprintf("PR %d base may be on trunk after retarget PATCH: %v", pr, cause),
		fix:    retargetBaseFix,
		code:   3,
		cause:  cause,
	}
}

func (r *repository) restoreRetargetBase(ctx context.Context, pr int, base string) error {
	s, err := service(ctx)
	if err != nil {
		return err
	}
	if err := s.forge.api(ctx, http.MethodPatch, prPath(r.profile.Name, pr), map[string]string{"base": base}, nil); err != nil {
		return err
	}
	after, err := s.forge.metadata(ctx, r.profile.Name, pr)
	if err != nil {
		return err
	}
	if after.BaseRef != base {
		return fmt.Errorf("restored base is %s, want %s", after.BaseRef, base)
	}
	return nil
}

func requestRetargetReview(ctx context.Context, reviewer, repo string, pr int, base string) error {
	s, err := service(ctx)
	if err != nil {
		return err
	}
	reviewErr := s.forge.api(ctx, http.MethodPost, prPath(repo, pr)+"/requested_reviewers", map[string]any{"reviewers": []string{reviewer}}, nil)
	commentErr := s.forge.api(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, pr), map[string]string{"body": "rjc retargeted this PR to " + base + "; please re-review."}, nil)
	return errors.Join(reviewErr, commentErr)
}
