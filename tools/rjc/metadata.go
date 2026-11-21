package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/samber/oops"
)

type TimelineEvent struct {
	Index                       int
	Event, State                string
	ReviewID, DismissedReviewID int64
}
type Coverage int

const (
	Uncovered Coverage = iota
	Covered
	Unknown
)

type GateState struct {
	PR       int
	Draft    bool
	Coverage Coverage
}

func ApprovalCoverage(events []TimelineEvent, complete bool) Coverage {
	if !complete {
		return Unknown
	}
	compare := func(a, b TimelineEvent) int { return cmp.Compare(a.Index, b.Index) }
	if !slices.IsSortedFunc(events, compare) {
		events = slices.Clone(events)
		slices.SortStableFunc(events, compare)
	}
	approvals := map[int64]bool{}
	known := map[int64]bool{}
	for _, event := range events {
		switch event.Event {
		case "committed", "head_ref_force_pushed":
			clear(approvals)
		case "reviewed":
			known[event.ReviewID] = true
			if strings.EqualFold(event.State, "approved") {
				approvals[event.ReviewID] = true
			}
		case "review_dismissed":
			if !known[event.DismissedReviewID] {
				return Unknown
			}
			delete(approvals, event.DismissedReviewID)
		}
	}
	if len(approvals) > 0 {
		return Covered
	}
	return Uncovered
}

func ReadGateState(ctx context.Context, repo string, pr int) (GateState, error) {
	s, err := service(ctx)
	if err != nil {
		return GateState{}, err
	}
	events, complete, timelineErr := s.forge.timeline(ctx, repo, pr)
	complete = complete && timelineErr == nil
	if timelineErr != nil && s.jj != nil && s.jj.log != nil {
		if _, err := fmt.Fprintf(s.jj.log, "Approval coverage is unknown: %v\n", timelineErr); err != nil {
			return GateState{}, oops.Wrapf(err, "write timeline diagnostic")
		}
	}
	metadata, readErr := s.forge.metadata(ctx, repo, pr)
	if readErr != nil {
		return GateState{}, readErr
	}
	return GateState{PR: pr, Draft: metadata.Draft, Coverage: ApprovalCoverage(events, complete)}, nil
}

func sameHeadBase(a, b Metadata) bool {
	return a.HeadRef == b.HeadRef && a.HeadSHA == b.HeadSHA && a.BaseRef == b.BaseRef && a.BaseSHA == b.BaseSHA
}

func ReconcileMetadata(ctx context.Context, repo string, pr int, baseline Published, ackHash string, want Metadata) error {
	s, err := service(ctx)
	if err != nil {
		return err
	}
	snapshot, err := s.forge.metadata(ctx, repo, pr)
	if err != nil {
		return err
	}
	if !sameHeadBase(snapshot, want) {
		return refusal("reconcile", "head-base-changed", "PR head or base differs from the compose snapshot", "rerun rjc submit with a fresh snapshot")
	}
	block, authored, err := splitBody(snapshot.Body)
	if err != nil {
		return err
	}
	if err := guardAuthored(baseline, baseline.Title != "", authored, ackHash); err != nil {
		return err
	}
	desiredBody := strings.TrimSpace(want.Body) + "\n"
	if block != "" {
		desiredBody = block + "\n\n" + desiredBody
	}
	fresh, err := s.forge.metadata(ctx, repo, pr)
	if err != nil {
		return err
	}
	if !sameHeadBase(fresh, snapshot) || fresh.Title != snapshot.Title || fresh.Body != snapshot.Body {
		_, remote, err := splitBody(fresh.Body)
		if err != nil {
			return err
		}
		if err := guardAuthored(baseline, baseline.Title != "", remote, ackHash); err != nil {
			return err
		}
		return refusal("reconcile", "remote-changed", "PR metadata changed before the write", "rerun rjc submit after merging the remote changes")
	}
	patch := map[string]string{}
	if fresh.Title != want.Title {
		patch["title"] = want.Title
	}
	if fresh.Body != desiredBody {
		patch["body"] = desiredBody
	}
	if len(patch) > 0 {
		if err := s.forge.api(ctx, http.MethodPatch, prPath(repo, pr), patch, nil); err != nil {
			return oops.Wrapf(err, "write PR metadata")
		}
	}
	confirmed, err := s.forge.metadata(ctx, repo, pr)
	if err != nil {
		return err
	}
	if confirmed.Title != want.Title || confirmed.Body != desiredBody || !sameHeadBase(confirmed, snapshot) {
		return refusal("reconcile", "read-back-mismatch", "PR read-back differs; published baseline is unchanged", "inspect the remote PR before rerunning rjc submit")
	}
	return oops.Wrapf(s.store.Commit(s.bookmark, Published{Title: want.Title, Authored: strings.TrimSpace(want.Body) + "\n"}), "publish confirmed baseline")
}

func ReRequestIfMoved(ctx context.Context, reviewer, repo string, pr int, covered bool, oldHead, newHead string) error {
	if !covered || oldHead == newHead {
		return nil
	}
	s, err := service(ctx)
	if err != nil {
		return err
	}
	reviewErr := s.forge.api(ctx, http.MethodPost, prPath(repo, pr)+"/requested_reviewers", map[string]any{"reviewers": []string{reviewer}}, nil)
	commentErr := s.forge.api(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, pr), map[string]string{"body": "Head moved to " + newHead + ". @" + reviewer + ": please review this head; the previous approval did not cover it."}, nil)
	return oops.Wrapf(errors.Join(reviewErr, commentErr), "re-request review after moving covered head")
}
