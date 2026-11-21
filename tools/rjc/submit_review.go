package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

const labelsKey = "labels"

func requestAgentReview(ctx context.Context, repo string, pr int, snapshot Metadata) error {
	if slices.Contains(snapshot.Labels, "review-requested") {
		return nil
	}
	s, err := service(ctx)
	if err != nil {
		return agentReviewFailure(pr, err)
	}
	path := fmt.Sprintf("/repos/%s/issues/%d/labels", repo, pr)
	payload := map[string][]string{labelsKey: {"review-requested"}}
	if err := s.forge.api(ctx, http.MethodPost, path, payload, nil); err != nil {
		return agentReviewFailure(pr, err)
	}
	return nil
}

// requestUpstreamSyncLabel adds upstream-sync so nemo-merge merge-commits the sync PR.
func requestUpstreamSyncLabel(ctx context.Context, repo string, pr int, snapshot Metadata) error {
	if slices.Contains(snapshot.Labels, "upstream-sync") {
		return nil
	}
	s, err := service(ctx)
	if err != nil {
		return upstreamSyncLabelFailure(pr, err)
	}
	path := fmt.Sprintf("/repos/%s/issues/%d/labels", repo, pr)
	payload := map[string][]string{labelsKey: {"upstream-sync"}}
	if err := s.forge.api(ctx, http.MethodPost, path, payload, nil); err != nil {
		return upstreamSyncLabelFailure(pr, err)
	}
	return nil
}

func upstreamSyncLabelFailure(pr int, cause error) error {
	return &failure{
		stage:  submitCommandName,
		gate:   toolFailureGate,
		reason: fmt.Sprintf("PR %d was pushed and reconciled, but the upstream-sync label was not added: %v", pr, cause),
		fix:    "rerun rjc submit to add the upstream-sync label",
		code:   3,
		cause:  cause,
	}
}

func agentReviewFailure(pr int, cause error) error {
	return &failure{
		stage:  submitCommandName,
		gate:   toolFailureGate,
		reason: fmt.Sprintf("PR %d was pushed and reconciled, but the review was not requested: %v", pr, cause),
		fix:    "rerun rjc submit --review",
		code:   3,
		cause:  cause,
	}
}

// requestDesignLabel adds design when every path the PR changes is under docs/designs/.
func (r *repository) requestDesignLabel(ctx context.Context, change Change, pr int, snapshot Metadata) error {
	if slices.Contains(snapshot.Labels, "design") {
		return nil
	}
	designOnly, err := r.designOnlyChange(ctx, change)
	if err != nil {
		return designLabelFailure(pr, err)
	}
	if !designOnly {
		return nil
	}
	s, err := service(ctx)
	if err != nil {
		return designLabelFailure(pr, err)
	}
	path := fmt.Sprintf("/repos/%s/issues/%d/labels", r.profile.Name, pr)
	payload := map[string][]string{labelsKey: {"design"}}
	if err := s.forge.api(ctx, http.MethodPost, path, payload, nil); err != nil {
		return designLabelFailure(pr, err)
	}
	return nil
}

// designOnlyChange reports whether change has at least one path and every path is under docs/designs/.
func (r *repository) designOnlyChange(ctx context.Context, change Change) (bool, error) {
	fork, err := r.validationBase(ctx, change.Base, change.Tip)
	if err != nil {
		return false, err
	}
	paths, err := r.changedPaths(ctx, fork, change.Tip)
	if err != nil {
		return false, err
	}
	if len(paths) == 0 {
		return false, nil
	}
	for _, path := range paths {
		if !strings.HasPrefix(path, "docs/designs/") {
			return false, nil
		}
	}
	return true, nil
}

func designLabelFailure(pr int, cause error) error {
	return &failure{
		stage:  submitCommandName,
		gate:   toolFailureGate,
		reason: fmt.Sprintf("PR %d was pushed and reconciled, but the design label was not added: %v", pr, cause),
		fix:    "rerun rjc submit to add the design label",
		code:   3,
		cause:  cause,
	}
}
