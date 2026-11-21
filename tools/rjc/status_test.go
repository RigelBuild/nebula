package main

import "testing"

func TestNextAction(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		row                               StatusRow
		parentLanded, mainRed, promotable bool
		want                              string
	}{
		{name: "merged", row: StatusRow{State: "merged"}, want: "rjc retire"},
		{name: "closed", row: StatusRow{State: "closed"}, want: "rjc retire"},
		{name: "landed parent", parentLanded: true, want: "rjc sync"},
		{name: "conflicting", row: StatusRow{Conflicts: "conflicting"}, want: "rjc sync"},
		{name: "eligible skip", row: StatusRow{Hooks: "skipped"}, mainRed: true, want: "rjc ready --main-red"},
		{name: "eligible partial", row: StatusRow{Hooks: "partial"}, mainRed: true, want: "rjc ready --main-red"},
		{name: "ineligible skip", row: StatusRow{Hooks: "skipped"}, want: "rjc submit"},
		{name: "free-text skip is promotable", row: StatusRow{State: "draft", Hooks: "skipped", Review: "all-clear"}, promotable: true, want: "rjc ready"},
		{name: "promotable skip still needs review", row: StatusRow{State: "draft", Hooks: "skipped", Review: "none"}, promotable: true, want: "run the review loop"},
		{name: "promotable skip hands off when ready", row: StatusRow{State: "ready", Hooks: "skipped", Review: "all-clear"}, promotable: true, want: "hand off"},
		{name: "missing hooks", row: StatusRow{Hooks: "none"}, want: "rjc submit"},
		{name: "missing review", row: StatusRow{Hooks: "pass", Review: "none"}, want: "run the review loop"},
		{name: "stale review", row: StatusRow{Hooks: "pass", Review: "stale"}, want: "run the review loop"},
		{name: "draft ready", row: StatusRow{State: "draft", Hooks: "pass", Review: "all-clear"}, want: "rjc ready"},
		{name: "ready handoff", row: StatusRow{State: "ready", Hooks: "pass", Review: "all-clear"}, want: "hand off"},
		{name: "main red handoff", row: StatusRow{State: "ready", Hooks: "main-red", Review: "all-clear"}, want: "hand off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextAction(tc.row, tc.parentLanded, tc.mainRed, tc.promotable); got != tc.want {
				t.Errorf("nextAction() = %q, want %q", got, tc.want)
			}
		})
	}
}
