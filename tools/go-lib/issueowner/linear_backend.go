// SPDX-License-Identifier: MIT OR Apache-2.0

package issueowner

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"regexp"
	"strings"
)

var (
	linearStateTypes = map[string]StateType{
		"backlog": StateBacklog, "unstarted": StateUnstarted, "started": StateStarted,
		"completed": StateCompleted, "canceled": StateCanceled, "triage": StateBacklog,
		"duplicate": StateCanceled,
	}
	linearStartedStateNames = map[string]BoardStatus{
		"In Progress": BoardInProgress, "In Review": BoardInReview, "Queued": BoardQueued,
	}
)

// ToOwnerStateType maps a Linear state type into the neutral state vocabulary.
func ToOwnerStateType(linearType string) StateType {
	if state, ok := linearStateTypes[linearType]; ok {
		return state
	}
	return StateBacklog
}

// ToBoardStatus maps Linear's coarse type and state name into a board stage.
func ToBoardStatus(linearType, linearName string) BoardStatus {
	switch ToOwnerStateType(linearType) {
	case StateBacklog:
		return BoardBacklog
	case StateUnstarted:
		return BoardQueued
	case StateCompleted:
		return BoardDone
	case StateCanceled:
		return BoardCanceled
	case StateStarted:
		if status, ok := linearStartedStateNames[linearName]; ok {
			return status
		}
		return BoardInProgress
	default:
		return BoardBacklog
	}
}

// LinearIssueFields is the subset of issue data retained by the seam.
type LinearIssueFields struct {
	Identifier       string
	Title            string
	URL              string
	Description      *string
	StateName        string
	StateType        string
	ParentIdentifier string
	CompletedAt      *string
}

type linearRaw struct{ body string }

// ToIssueRecord maps a Linear issue into a backend-neutral record.
func ToIssueRecord(fields LinearIssueFields) IssueRecord {
	record := IssueRecord{
		Identifier: fields.Identifier, Title: fields.Title, URL: fields.URL,
		StateType:     ToOwnerStateType(fields.StateType),
		BoardStatus:   ToBoardStatus(fields.StateType, fields.StateName),
		DisplayStatus: fields.StateName, Raw: linearRaw{},
	}
	if fields.Description != nil {
		record.Raw = linearRaw{body: *fields.Description}
	}
	if fields.CompletedAt != nil && *fields.CompletedAt != "" {
		record.CompletedAt = fields.CompletedAt
	}
	if fields.ParentIdentifier != "" {
		record.Parent = &IssueParent{Identifier: fields.ParentIdentifier}
	}
	return record
}

// LinearIssueAPI is the HTTP-independent surface used by the backend.
type LinearIssueAPI interface {
	FetchIssue(ctx context.Context, identifier string) (LinearIssueFields, error)
	FetchPage(ctx context.Context, after string) (LinearPage, error)
	UpdateDescription(ctx context.Context, identifier, description string) error
}

// LinearPage contains a page of issues and the cursor returned by Linear.
type LinearPage struct {
	Issues    []LinearIssueFields
	EndCursor string
}

var issueRefPattern = regexp.MustCompile(`(?i)\b(?:SEA|RIG)-\d+\b`)

// LinearBackend implements the owner seam for Linear records.
type LinearBackend struct{ api LinearIssueAPI }

// NewLinearBackend creates a backend over the supplied API implementation.
func NewLinearBackend(api LinearIssueAPI) *LinearBackend { return &LinearBackend{api: api} }

// IssueRefPattern returns the case-insensitive Linear issue reference pattern.
func (b *LinearBackend) IssueRefPattern() *regexp.Regexp { return issueRefPattern }

// CanonicalRef maps historical SEA references to their current RIG identifiers.
func (b *LinearBackend) CanonicalRef(ref string) string {
	upper := strings.ToUpper(ref)
	if strings.HasPrefix(upper, "SEA-") {
		return "RIG-" + upper[len("SEA-"):]
	}
	return upper
}

type linearMutation struct{ payload *mutationPayload }

func (linearMutation) mutation() {}

type mutationPayload struct {
	backend    *LinearBackend
	identifier string
	bodyBefore string
	bodyAfter  string
	err        error
}

var _ Backend = (*LinearBackend)(nil)

// ReadOwner reads a body stored in the Linear-private record payload.
func (b *LinearBackend) ReadOwner(issue IssueRecord) (string, bool) {
	raw, ok := issue.Raw.(linearRaw)
	if !ok {
		return "", false
	}
	return ReadOwnerFromBody(raw.body)
}

// ReadOwnerDiagnostic reports malformed owner text in the Linear-private payload.
func (b *LinearBackend) ReadOwnerDiagnostic(issue IssueRecord) (Diagnostic, bool) {
	raw, ok := issue.Raw.(linearRaw)
	if !ok {
		return Diagnostic{}, false
	}
	return ReadOwnerDiagnosticFromBody(raw.body)
}

// BoardStatusWarning reports unknown Linear started-state names.
func (b *LinearBackend) BoardStatusWarning(issue IssueRecord) (string, bool) {
	if issue.StateType != StateStarted {
		return "", false
	}
	if _, ok := linearStartedStateNames[issue.DisplayStatus]; ok {
		return "", false
	}
	return fmt.Sprintf(`unmapped Linear started-state name %q → boardStatus in_progress; add it to LINEAR_STARTED_STATE_NAMES if it needs its own column`, issue.DisplayStatus), true
}

// WriteOwner records a body mutation; Apply reports invalid records without panicking.
func (b *LinearBackend) WriteOwner(issue IssueRecord, owner string) Mutation {
	raw, ok := issue.Raw.(linearRaw)
	if !ok {
		return linearMutation{payload: &mutationPayload{backend: b, identifier: issue.Identifier, err: fmt.Errorf("writeOwner: %s did not come from this backend (raw has no body)", issue.Identifier)}}
	}
	return linearMutation{payload: &mutationPayload{backend: b, identifier: issue.Identifier, bodyBefore: raw.body, bodyAfter: WriteOwnerInBody(raw.body, owner)}}
}

// Apply sends a changed body through the API and skips byte-identical updates.
func (b *LinearBackend) Apply(ctx context.Context, handle Mutation) error {
	linearHandle, ok := handle.(linearMutation)
	if !ok || linearHandle.payload == nil || linearHandle.payload.backend != b {
		return errors.New("apply: unrecognised Mutation — mutations must come from this backend's WriteOwner")
	}
	payload := linearHandle.payload
	if payload.err != nil {
		return payload.err
	}
	if payload.bodyAfter == payload.bodyBefore {
		return nil
	}
	if b.api == nil {
		return errors.New("apply: Linear API is not configured")
	}
	if err := b.api.UpdateDescription(ctx, payload.identifier, payload.bodyAfter); err != nil {
		return fmt.Errorf("applying owner mutation for %s: %w", payload.identifier, err)
	}
	return nil
}

// Reread fetches one issue fresh through the configured API.
func (b *LinearBackend) Reread(ctx context.Context, identifier string) (IssueRecord, error) {
	if b.api == nil {
		return IssueRecord{}, errors.New("rereading Linear issue: API is not configured")
	}
	fields, err := b.api.FetchIssue(ctx, identifier)
	if err != nil {
		return IssueRecord{}, fmt.Errorf("rereading Linear issue %s: %w", identifier, err)
	}
	return ToIssueRecord(fields), nil
}

// Scan yields all Linear issues across pages, stopping after the first error.
func (b *LinearBackend) Scan(ctx context.Context) iter.Seq2[IssueRecord, error] {
	return func(yield func(IssueRecord, error) bool) {
		if b.api == nil {
			yield(IssueRecord{}, errors.New("scanning Linear issues: API is not configured"))
			return
		}
		cursor := ""
		for {
			page, err := b.api.FetchPage(ctx, cursor)
			if err != nil {
				yield(IssueRecord{}, fmt.Errorf("scanning Linear issues: %w", err))
				return
			}
			for _, fields := range page.Issues {
				if !yield(ToIssueRecord(fields), nil) {
					return
				}
			}
			if page.EndCursor == "" {
				return
			}
			cursor = page.EndCursor
		}
	}
}
