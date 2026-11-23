package issueowner

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func testFields(overrides LinearIssueFields) LinearIssueFields {
	fields := LinearIssueFields{
		Identifier:  "EX-1317",
		Title:       "Issue title",
		URL:         "https://linear.app/example/issue/EX-1317/example",
		Description: new("Owner: ci-build\n\nBody."),
		StateName:   "Backlog",
		StateType:   "backlog",
	}
	if overrides.Identifier != "" {
		fields.Identifier = overrides.Identifier
	}
	if overrides.Title != "" {
		fields.Title = overrides.Title
	}
	if overrides.URL != "" {
		fields.URL = overrides.URL
	}
	if overrides.Description != nil {
		fields.Description = overrides.Description
	}
	if overrides.StateName != "" {
		fields.StateName = overrides.StateName
	}
	if overrides.StateType != "" {
		fields.StateType = overrides.StateType
	}
	if overrides.ParentIdentifier != "" {
		fields.ParentIdentifier = overrides.ParentIdentifier
	}
	if overrides.CompletedAt != nil {
		fields.CompletedAt = overrides.CompletedAt
	}
	return fields
}

type fakeLinearAPI struct {
	issue    LinearIssueFields
	pages    map[string]LinearPage
	fetches  []string
	writes   [][2]string
	fetchErr error
	pageErr  error
	writeErr error
}

func (f *fakeLinearAPI) FetchIssue(_ context.Context, id string) (LinearIssueFields, error) {
	f.fetches = append(f.fetches, id)
	if f.fetchErr != nil {
		return LinearIssueFields{}, f.fetchErr
	}
	issue := f.issue
	issue.Identifier = id
	return issue, nil
}

func (f *fakeLinearAPI) FetchPage(_ context.Context, after string) (LinearPage, error) {
	f.fetches = append(f.fetches, after)
	if f.pageErr != nil {
		return LinearPage{}, f.pageErr
	}
	return f.pages[after], nil
}

func (f *fakeLinearAPI) UpdateDescription(_ context.Context, id, body string) error {
	f.writes = append(f.writes, [2]string{id, body})
	return f.writeErr
}

func TestToOwnerStateType(t *testing.T) {
	tests := []struct {
		name, input string
		want        StateType
	}{
		{name: "backlog", input: "backlog", want: StateBacklog},
		{name: "unstarted", input: "unstarted", want: StateUnstarted},
		{name: "started", input: "started", want: StateStarted},
		{name: "completed", input: "completed", want: StateCompleted},
		{name: "canceled", input: "canceled", want: StateCanceled},
		{name: "triage", input: "triage", want: StateBacklog},
		{name: "duplicate", input: "duplicate", want: StateCanceled},
		{name: "future type", input: "some-future-type", want: StateBacklog},
		{name: "empty", input: "", want: StateBacklog},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ToOwnerStateType(test.input); got != test.want {
				t.Fatalf("ToOwnerStateType(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
	for _, kind := range []string{"triage", "backlog", "unstarted", "started", "completed", "canceled", "duplicate"} {
		if _, ok := linearStateTypes[kind]; !ok {
			t.Errorf("state map lacks %q", kind)
		}
	}
}
func TestToBoardStatus(t *testing.T) {
	tests := []struct {
		name, stateType, stateName string
		want                       BoardStatus
	}{
		{name: "in review", stateType: "started", stateName: "In Review", want: BoardInReview},
		{name: "in progress", stateType: "started", stateName: "In Progress", want: BoardInProgress},
		{name: "queued", stateType: "started", stateName: "Queued", want: BoardQueued},
		{name: "unstarted", stateType: "unstarted", stateName: "Todo", want: BoardQueued},
		{name: "backlog", stateType: "backlog", stateName: "Backlog", want: BoardBacklog},
		{name: "triage", stateType: "triage", stateName: "Triage", want: BoardBacklog},
		{name: "completed", stateType: "completed", stateName: "Done", want: BoardDone},
		{name: "canceled", stateType: "canceled", stateName: "Canceled", want: BoardCanceled},
		{name: "duplicate", stateType: "duplicate", stateName: "Duplicate", want: BoardCanceled},
		{name: "unmapped started", stateType: "started", stateName: "Awaiting QA", want: BoardInProgress},
		{name: "prototype name", stateType: "started", stateName: "constructor", want: BoardInProgress},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ToBoardStatus(test.stateType, test.stateName); got != test.want {
				t.Fatalf("ToBoardStatus() = %q, want %q", got, test.want)
			}
		})
	}
	if linearStartedStateNames["In Review"] != BoardInReview || linearStartedStateNames["Queued"] != BoardQueued {
		t.Fatal("started-state map lost its non-default entries")
	}
}

func TestToIssueRecord(t *testing.T) {
	tests := []struct {
		name        string
		fields      LinearIssueFields
		state       StateType
		status      BoardStatus
		completedAt *string
		parent      string
	}{
		{name: "triage mapped to neutral state", fields: testFields(LinearIssueFields{StateName: "Triage", StateType: "triage"}), state: StateBacklog, status: BoardBacklog},
		{name: "review retains coarse and fine state", fields: testFields(LinearIssueFields{StateName: "In Review", StateType: "started"}), state: StateStarted, status: BoardInReview},
		{name: "completed timestamp", fields: testFields(LinearIssueFields{StateName: "Done", StateType: "completed", CompletedAt: new("2026-08-20T10:00:00.000Z")}), state: StateCompleted, status: BoardDone, completedAt: new("2026-08-20T10:00:00.000Z")},
		{name: "parent", fields: testFields(LinearIssueFields{ParentIdentifier: "EX-1300"}), state: StateBacklog, status: BoardBacklog, parent: "EX-1300"},
		{name: "empty completion timestamp omitted", fields: testFields(LinearIssueFields{StateName: "Done", StateType: "completed", CompletedAt: new("")}), state: StateCompleted, status: BoardDone},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ToIssueRecord(test.fields)
			parent := ""
			if got.Parent != nil {
				parent = got.Parent.Identifier
			}
			if got.StateType != test.state || got.BoardStatus != test.status || got.DisplayStatus != test.fields.StateName || !reflect.DeepEqual(got.CompletedAt, test.completedAt) || parent != test.parent {
				t.Fatalf("ToIssueRecord() = %#v", got)
			}
		})
	}
}
func TestLinearBackendReadAndWarning(t *testing.T) {
	backend := NewLinearBackend(nil)
	tests := []struct {
		name    string
		fields  LinearIssueFields
		owner   string
		ownerOK bool
		warning bool
	}{
		{name: "body owner", fields: testFields(LinearIssueFields{}), owner: "ci-build", ownerOK: true},
		{name: "malformed owner", fields: testFields(LinearIssueFields{Description: new("Owner: CI-build\n\nBody.")})},
		{name: "unknown started name", fields: testFields(LinearIssueFields{StateName: "Awaiting QA", StateType: "started"}), owner: "ci-build", ownerOK: true, warning: true},
		{name: "prototype started name", fields: testFields(LinearIssueFields{StateName: "constructor", StateType: "started"}), owner: "ci-build", ownerOK: true, warning: true},
		{name: "known names", fields: testFields(LinearIssueFields{StateName: "In Progress", StateType: "started"}), owner: "ci-build", ownerOK: true},
		{name: "non-started", fields: testFields(LinearIssueFields{StateName: "Backlog", StateType: "backlog"}), owner: "ci-build", ownerOK: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := ToIssueRecord(test.fields)
			owner, ok := backend.ReadOwner(record)
			if owner != test.owner || ok != test.ownerOK {
				t.Fatalf("ReadOwner() = (%q, %t), want (%q, %t)", owner, ok, test.owner, test.ownerOK)
			}
			diagnostic, hasDiagnostic := backend.ReadOwnerDiagnostic(record)
			if hasDiagnostic != (test.name == "malformed owner") || hasDiagnostic && diagnostic.Kind != OwnerDiagnosticMalformed {
				t.Fatalf("ReadOwnerDiagnostic() = (%#v, %t)", diagnostic, hasDiagnostic)
			}
			warning, hasWarning := backend.BoardStatusWarning(record)
			if hasWarning != test.warning {
				t.Fatalf("BoardStatusWarning() = (%q, %t), want warning=%t", warning, hasWarning, test.warning)
			}
		})
	}
	for _, ref := range []string{"RIG-42", "rig-42", "EX-42", "ex-42"} {
		if got := backend.CanonicalRef(ref); got != strings.ToUpper(ref) {
			t.Errorf("CanonicalRef(%q) = %q, want %q", ref, got, strings.ToUpper(ref))
		}
	}
	legacyRef := "S" + "EA-42"
	if got := backend.CanonicalRef(legacyRef); got != legacyRef {
		t.Fatalf("CanonicalRef rewrote an unsupported reference: %q", got)
	}
	if backend.IssueRefPattern().MatchString("branch " + legacyRef) {
		t.Fatal("IssueRefPattern matched an unsupported reference")
	}
	if backend.IssueRefPattern().MatchString("branch EX-42") {
		t.Fatal("IssueRefPattern matched a non-RIG reference")
	}
	if got := backend.IssueRefPattern().FindString("branch rig-1182"); got != "rig-1182" {
		t.Fatalf("IssueRefPattern did not match lowercase ref: %q", got)
	}
	if len(backend.IssueRefPattern().SubexpNames()) != 1 {
		t.Fatal("issue reference pattern must not have capture groups")
	}
	foreign := IssueRecord{Identifier: "X-1", Raw: struct{ notBody bool }{true}}
	if _, ok := backend.ReadOwner(foreign); ok {
		t.Fatal("foreign record read as owned")
	}
	if _, ok := backend.ReadOwnerDiagnostic(foreign); ok {
		t.Fatal("foreign record produced a diagnostic")
	}
}

func TestLinearBackendMutations(t *testing.T) {
	api := &fakeLinearAPI{}
	var backend Backend = NewLinearBackend(api)
	record := ToIssueRecord(testFields(LinearIssueFields{Description: new("Body."), Identifier: "EX-1"}))
	mutation := backend.WriteOwner(record, "nix-infra")
	if err := backend.Apply(t.Context(), mutation); err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"EX-1", "Owner: nix-infra\n\nBody."}}
	if !reflect.DeepEqual(api.writes, want) {
		t.Fatalf("writes = %#v, want %#v", api.writes, want)
	}
	if err := backend.Apply(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "unrecognised") {
		t.Fatalf("nil mutation error = %v", err)
	}
	api.writes = nil
	noop := backend.WriteOwner(ToIssueRecord(testFields(LinearIssueFields{Description: new("Owner: ci-build\n\nBody.")})), "ci-build")
	if err := backend.Apply(t.Context(), noop); err != nil {
		t.Fatal(err)
	}
	if len(api.writes) != 0 {
		t.Fatalf("no-op caused API calls: %#v", api.writes)
	}
	held := []Mutation{
		backend.WriteOwner(ToIssueRecord(testFields(LinearIssueFields{Identifier: "EX-2", Description: new("A.")})), "ci-build"),
		backend.WriteOwner(ToIssueRecord(testFields(LinearIssueFields{Identifier: "EX-3", Description: new("B.")})), "nix-infra"),
	}
	for _, item := range held {
		if err := backend.Apply(t.Context(), item); err != nil {
			t.Fatal(err)
		}
	}
	if len(api.writes) != 2 || api.writes[0][0] != "EX-2" || api.writes[1][0] != "EX-3" {
		t.Fatalf("held mutations = %#v", api.writes)
	}
	foreign := backend.WriteOwner(IssueRecord{Identifier: "X-1"}, "ci-build")
	if err := backend.Apply(t.Context(), foreign); err == nil || !strings.Contains(err.Error(), "did not come from this backend") {
		t.Fatalf("foreign record error = %v", err)
	}
	api.writeErr = errors.New("write failed")
	if err := backend.Apply(t.Context(), backend.WriteOwner(record, "agents")); err == nil || !errors.Is(err, api.writeErr) {
		t.Fatalf("write error was not wrapped: %v", err)
	}
}

func TestLinearBackendRereadAndScan(t *testing.T) {
	api := &fakeLinearAPI{
		issue: testFields(LinearIssueFields{Description: new("Owner: nix-infra\n\nFresh.")}),
		pages: map[string]LinearPage{
			"":   {Issues: []LinearIssueFields{testFields(LinearIssueFields{Identifier: "EX-1"}), testFields(LinearIssueFields{Identifier: "EX-2"})}, EndCursor: "c1"},
			"c1": {Issues: []LinearIssueFields{testFields(LinearIssueFields{Identifier: "EX-3", StateType: "completed", StateName: "Done"})}, EndCursor: "c2"},
			"c2": {Issues: []LinearIssueFields{testFields(LinearIssueFields{Identifier: "EX-4", StateType: "canceled", StateName: "Canceled"})}},
		},
	}
	var backend Backend = NewLinearBackend(api)
	record, err := backend.Reread(t.Context(), "EX-1197")
	if err != nil || record.Identifier != "EX-1197" {
		t.Fatalf("Reread() = (%#v, %v)", record, err)
	}
	if owner, ok := backend.ReadOwner(record); !ok || owner != "nix-infra" {
		t.Fatalf("reread owner = %q, %t", owner, ok)
	}
	var identifiers []string
	var states []StateType
	for record, err := range backend.Scan(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		identifiers = append(identifiers, record.Identifier)
		states = append(states, record.StateType)
	}
	if !reflect.DeepEqual(identifiers, []string{"EX-1", "EX-2", "EX-3", "EX-4"}) {
		t.Fatalf("scan identifiers = %#v", identifiers)
	}
	if !reflect.DeepEqual(states, []StateType{StateBacklog, StateBacklog, StateCompleted, StateCanceled}) {
		t.Fatalf("scan states = %#v", states)
	}
	if !reflect.DeepEqual(api.fetches, []string{"EX-1197", "", "c1", "c2"}) {
		t.Fatalf("API calls = %#v", api.fetches)
	}
	api.pages = map[string]LinearPage{}
	api.fetches = nil
	for record, err := range backend.Scan(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		t.Errorf("empty scan yielded %#v", record)
	}
	if len(api.fetches) != 1 {
		t.Fatalf("empty scan calls = %#v", api.fetches)
	}
}
