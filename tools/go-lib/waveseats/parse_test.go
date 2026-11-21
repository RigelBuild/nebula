// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import (
	"strings"
	"testing"
)

func TestValidSeatTimestamp(t *testing.T) {
	tests := []struct {
		name  string
		value string
		valid bool
	}{
		{name: "normal UTC", value: "2026-01-01T00:00:00Z", valid: true},
		{name: "fractional seconds", value: "2026-01-01T00:00:00.12345678901234567890Z", valid: true},
		{name: "day overflow normalizes", value: "2026-02-30T00:00:00Z", valid: true},
		{name: "last day overflow normalizes", value: "2026-02-31T00:00:00Z", valid: true},
		{name: "hour 24 normalizes", value: "2026-01-01T24:00:00Z", valid: true},
		{name: "zero fractional hour 24 normalizes", value: "2026-01-01T24:00:00.000000000Z", valid: true},
		{name: "month zero", value: "2026-00-01T00:00:00Z"},
		{name: "month 13", value: "2026-13-01T00:00:00Z"},
		{name: "day zero", value: "2026-01-00T00:00:00Z"},
		{name: "day 32", value: "2026-01-32T00:00:00Z"},
		{name: "hour 25", value: "2026-01-01T25:00:00Z"},
		{name: "hour 24 with minute", value: "2026-01-01T24:01:00Z"},
		{name: "hour 24 with second", value: "2026-01-01T24:00:01Z"},
		{name: "hour 24 with fractional millisecond", value: "2026-01-01T24:00:00.0001Z"},
		{name: "minute 60", value: "2026-01-01T00:60:00Z"},
		{name: "second 60", value: "2026-01-01T00:00:60Z"},
		{name: "fraction requires digits", value: "2026-01-01T00:00:00.Z"},
		{name: "UTC offset is outside the accepted format", value: "2026-01-01T00:00:00+00:00"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validSeatTimestamp(test.value); got != test.valid {
				t.Fatalf("validSeatTimestamp(%q) = %t, want %t", test.value, got, test.valid)
			}
		})
	}
}

func TestParseSeatListV2(t *testing.T) {
	t.Run("accepts a closed v2 list", func(t *testing.T) {
		text, err := SerializeSeatListV2(v2TestList(1, "alpha"), nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseSeatListV2(string(text), nil)
		if err != nil || got.Seats[0].Name != "alpha" {
			t.Fatalf("parse = %#v, %v", got, err)
		}
	})
	t.Run("refuses v1", func(t *testing.T) {
		_, err := ParseSeatListV2(`{"version":1,"seats":[]}`, nil)
		if err == nil || !strings.Contains(err.Error(), "version must be 2") {
			t.Fatalf("error = %v", err)
		}
	})
	for _, test := range []struct{ name, text, want string }{
		{"unknown top-level key", `{"version":2,"revision":1,"seats":[],"extra":true}`, `unknown key "extra"`},
		{"unknown seat key", v2TestListWithRaw(`"surprise":1`), `unknown key "surprise"`},
		{"unknown key settings key", v2TestListWithKey(`"extra":1`), `unknown key "extra"`},
		{"unknown model limit key", v2TestListWithModelLimit(`"bogus":2`), `unknown key "bogus"`},
		{"duplicate names", v2TestDuplicate(), "is duplicated"},
		{"relative cwd", v2TestListWithCWD("relative/path"), "must be absolute"},
		{"whitespace-only tab", v2TestListWithTab(" "), "must not be blank or padded"},
		{"padded reserved tab", v2TestListWithTab("_hold "), "must not be blank or padded"},
		{"padded tab", v2TestListWithTab(" agents"), "must not be blank or padded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseSeatListV2(test.text, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestV2ParentValidation(t *testing.T) {
	parent := func(value string) *string { return &value }
	for _, test := range []struct {
		name  string
		seats []SeatEntryV2
		want  string
	}{
		{name: "valid chain", seats: []SeatEntryV2{{Name: "alice", Parent: parent("bob")}, {Name: "bob"}}},
		{name: "self", seats: []SeatEntryV2{{Name: "alice", Parent: parent("alice")}}, want: "own parent"},
		{name: "unknown", seats: []SeatEntryV2{{Name: "alice", Parent: parent("missing")}}, want: "dangling parent"},
		{name: "foreman parent", seats: []SeatEntryV2{{Name: "foreman", Parent: parent("alice")}, {Name: "alice"}}, want: "takes no parent"},
		{name: "foreman as parent", seats: []SeatEntryV2{{Name: "alice", Parent: parent("foreman")}, {Name: "foreman"}}, want: "root foreman as parent"},
		{name: "supervisor is an ordinary seat with a parent", seats: []SeatEntryV2{{Name: "supervisor", Parent: parent("alice")}, {Name: "alice"}}},
		{name: "cycle", seats: []SeatEntryV2{{Name: "alice", Parent: parent("bob")}, {Name: "bob", Parent: parent("alice")}}, want: "cycle"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateV2Parents(test.seats)
			if test.want == "" && err != nil {
				t.Fatal(err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("error = %v, want %s", err, test.want)
			}
		})
	}
}

func v2TestListWithRaw(extra string) string {
	return `{"version":2,"revision":1,"seats":[{"name":"alpha","seatRevision":1,"cwd":"/work/alpha","tab":"alpha","state":"active","restartGeneration":0,"updatedAt":"` + v2TestStamp + `","profile":"worker","role":"worker","roleTreeRevision":0,"key":{"routing":{},"limits":{},"policyId":"policy-a"},"workspace":"/work/alpha","persona":"alpha",` + extra + `}]}`
}

func v2TestListWithKey(extra string) string {
	return strings.Replace(v2TestListWithRaw(`"x":1`), `"x":1`, `"key":{"routing":{},"limits":{},"policyId":"p",`+extra+`}`, 1)
}

func v2TestListWithModelLimit(extra string) string {
	return strings.Replace(v2TestListWithRaw(`"x":1`), `"x":1`, `"key":{"routing":{},"limits":{"perModel":{"m":{"tokens":1,`+extra+`}}},"policyId":"p"}`, 1)
}

func v2TestListWithCWD(cwd string) string {
	return strings.Replace(v2TestListWithRaw(`"x":1`), `"x":1`, `"cwd":"`+cwd+`"`, 1)
}

func v2TestListWithTab(tab string) string {
	seat := `{"name":"alpha","seatRevision":1,"cwd":"/work/alpha","tab":"` + tab + `","state":"active","restartGeneration":0,"updatedAt":"` + v2TestStamp + `","profile":"worker","role":"worker","roleTreeRevision":0,"key":{"routing":{},"limits":{},"policyId":"policy-a"},"workspace":"/work/alpha","persona":"alpha"}`
	return `{"version":2,"revision":1,"seats":[` + seat + `]}`
}

func v2TestDuplicate() string {
	seat := `{"name":"alpha","seatRevision":1,"cwd":"/work/alpha","tab":"alpha","state":"active","restartGeneration":0,"updatedAt":"` + v2TestStamp + `","profile":"worker","role":"worker","roleTreeRevision":0,"key":{"routing":{},"limits":{},"policyId":"policy-a"},"workspace":"/work/alpha","persona":"alpha"}`
	return `{"version":2,"revision":1,"seats":[` + seat + `,` + seat + `]}`
}
