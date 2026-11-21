// SPDX-License-Identifier: MIT OR Apache-2.0

// Package issueowner parses Linear owner markers and exposes a tracker-neutral backend seam.
package issueowner

import (
	"context"
	"iter"
	"regexp"
)

// StateType is the coarse, backend-neutral issue state.
type StateType string

const (
	StateBacklog   StateType = "backlog"
	StateUnstarted StateType = "unstarted"
	StateStarted   StateType = "started"
	StateCompleted StateType = "completed"
	StateCanceled  StateType = "canceled"
)

// BoardStatus is the finer, backend-neutral workflow stage.
type BoardStatus string

const (
	BoardBacklog    BoardStatus = "backlog"
	BoardQueued     BoardStatus = "queued"
	BoardInProgress BoardStatus = "in_progress"
	BoardInReview   BoardStatus = "in_review"
	BoardDone       BoardStatus = "done"
	BoardCanceled   BoardStatus = "canceled"
)

// IssueRecord contains the fields shared by issue-owner backends.
type IssueRecord struct {
	Identifier    string
	Title         string
	URL           string
	StateType     StateType
	BoardStatus   BoardStatus
	DisplayStatus string
	CompletedAt   *string
	Parent        *IssueParent
	Raw           any
}

// IssueParent contains the parent identifier exposed by issue-owner backends.
type IssueParent struct {
	Identifier string
}

// Diagnostic describes an owner line that looks intentional but cannot be parsed.
type Diagnostic struct {
	Kind   string
	Line   string
	Detail string
}

// Mutation is an opaque backend-specific handle passed through lifecycle methods.
type Mutation interface {
	mutation()
}

// Backend provides owner operations without exposing tracker-specific data.
type Backend interface {
	IssueRefPattern() *regexp.Regexp
	CanonicalRef(ref string) string
	ReadOwner(issue IssueRecord) (string, bool)
	ReadOwnerDiagnostic(issue IssueRecord) (Diagnostic, bool)
	BoardStatusWarning(issue IssueRecord) (string, bool)
	WriteOwner(issue IssueRecord, owner string) Mutation
	Apply(ctx context.Context, mutation Mutation) error
	Reread(ctx context.Context, identifier string) (IssueRecord, error)
	Scan(ctx context.Context) iter.Seq2[IssueRecord, error]
}
