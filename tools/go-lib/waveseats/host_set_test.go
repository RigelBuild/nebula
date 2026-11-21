// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import (
	"os"
	"strings"
	"testing"
)

// hostedSeatListText returns a v2 seat list whose one seat names host.
func hostedSeatListText(t *testing.T, host SeatHost) string {
	t.Helper()
	list := v2TestList(1, "alpha")
	list.Seats[0].Host = &host
	text, err := SerializeSeatListV2(list, HostSet{host: {}})
	if err != nil {
		t.Fatalf("serialize hosted list: %v", err)
	}
	return string(text)
}

func TestParseSeatListV2HostMembership(t *testing.T) {
	const configured SeatHost = "build-a"
	const other SeatHost = "build-b"
	tests := []struct {
		name    string
		hosts   HostSet
		host    SeatHost
		wantErr string
	}{
		{name: "host in set is accepted", hosts: HostSet{configured: {}}, host: configured},
		{name: "host outside set is refused", hosts: HostSet{configured: {}}, host: other, wantErr: "must be one of the configured hosts: build-a"},
		{name: "empty set refuses every host", hosts: HostSet{}, host: configured, wantErr: "must be one of the configured hosts: (none configured)"},
		{name: "nil set refuses every host", hosts: nil, host: configured, wantErr: "must be one of the configured hosts: (none configured)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Serialize with the host allowed, so the fixture is well-formed; parse with the test's set.
			text := hostedSeatListText(t, test.host)
			_, err := ParseSeatListV2(text, test.hosts)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseSeatListV2 error=%v want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ParseSeatListV2 error=%v want %q", err, test.wantErr)
			}
		})
	}
}

func TestSerializeSeatListV2RefusesHostOutsideSet(t *testing.T) {
	host := SeatHost("build-a")
	list := v2TestList(1, "alpha")
	list.Seats[0].Host = &host
	if _, err := SerializeSeatListV2(list, HostSet{"build-b": {}}); err == nil || !strings.Contains(err.Error(), "must be one of the configured hosts") {
		t.Fatalf("SerializeSeatListV2 error=%v want host refusal", err)
	}
}

func TestValidateEntitlementMissingSeat(t *testing.T) {
	entitlements, err := os.ReadFile("testdata/entitlements.json")
	if err != nil {
		t.Fatal(err)
	}
	key := KeyRequest{Routing: map[Provider]any{ProviderClaude: "shared"}}
	err = ValidateEntitlement(string(entitlements), "unknown-seat", "build.coordination.lead", key)
	if err == nil || !strings.Contains(err.Error(), "permission-denied: missing seat entitlement") {
		t.Fatalf("ValidateEntitlement error=%v want missing seat entitlement", err)
	}
}
