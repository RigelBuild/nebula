// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import "testing"

func TestActiveRoot(t *testing.T) {
	cases := []struct {
		name  string
		seats []SeatEntryV2
		want  string
	}{
		{"foreman only", []SeatEntryV2{{Name: "foreman", State: SeatV2Active}, {Name: "supervisor", State: SeatV2Paused}}, "foreman"},
		{"active supervisor is not a root", []SeatEntryV2{{Name: "foreman", State: SeatV2Paused}, {Name: "supervisor", State: SeatV2Active}}, ""},
		{"neither active", []SeatEntryV2{{Name: "foreman", State: SeatV2Paused}, {Name: "supervisor", State: SeatV2Retired}}, ""},
		{"active supervisor does not displace foreman", []SeatEntryV2{{Name: "foreman", State: SeatV2Active}, {Name: "supervisor", State: SeatV2Active}}, "foreman"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ActiveRoot(tc.seats); got != tc.want {
				t.Fatalf("ActiveRoot() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestActiveRootNames(t *testing.T) {
	for _, test := range []struct {
		name  string
		seats map[string]struct{}
		want  string
	}{
		{name: "no root", seats: map[string]struct{}{"worker": {}}, want: ""},
		{name: "foreman fallback", seats: map[string]struct{}{"foreman": {}}, want: "foreman"},
		{name: "supervisor is not a root", seats: map[string]struct{}{"supervisor": {}}, want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ActiveRootNames(test.seats); got != test.want {
				t.Fatalf("ActiveRootNames() = %q, want %q", got, test.want)
			}
		})
	}
}
