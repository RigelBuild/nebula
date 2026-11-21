package main

import (
	"errors"
	"strings"
	"testing"
)

// F3: the inherited recheck accepts a failure only on a target the lower head's failed run named. A failure on any
// other target is the upper head's own failure and refuses main-red. A failing hook with no target report is also
// refused, so it can never be recorded as main-red without evidence.
func TestAcceptInheritedFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		failed []string
		lower  []string
		want   string
	}{
		{name: "failure only on a lower target is accepted", failed: []string{"a:test"}, lower: []string{"a:test"}},
		{name: "failure on a target the lower did not fail refuses", failed: []string{"c:real"}, lower: []string{"a:test"}, want: "outside the lower failed targets"},
		{name: "mixed failures refuse on the foreign target", failed: []string{"a:test", "c:real"}, lower: []string{"a:test"}, want: "outside the lower failed targets"},
		{name: "failed hook with no target report refuses", failed: nil, lower: []string{"a:test"}, want: "no failing target in the report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := acceptInheritedFailures(tc.failed, tc.lower)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("accepted failure refused: %v", err)
				}
				return
			}
			var refusal *failure
			if !errors.As(err, &refusal) || refusal.gate != "not-red-on-main" || !strings.Contains(refusal.reason, tc.want) {
				t.Fatalf("got %v, want not-red-on-main containing %q", err, tc.want)
			}
		})
	}
}
