// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import (
	"os"
	"strings"
	"testing"
)

func TestSeatListV1ImportParsing(t *testing.T) {
	text := `{"version":1,"seats":[{"name":"alpha","cwd":"/work/alpha","tab":"workers","state":"active","updatedAt":"` + v2TestStamp + `"}]}`
	legacy, err := ParseSeatListV1ForImport(text)
	if err != nil || len(legacy) != 1 || legacy[0].Name != "alpha" {
		t.Fatalf("legacy=%#v err=%v", legacy, err)
	}
	if _, err := ParseSeatListV1ForImport(`{"version":1,"seats":[],"extra":1}`); err == nil {
		t.Fatal("unknown v1 root field accepted")
	}
}

func TestV1ImportPreservesParent(t *testing.T) {
	legacy := `{"version":1,"seats":[{"name":"alice","cwd":"/work/alice","state":"active","updatedAt":"` + v2TestStamp + `","parent":"bob"},{"name":"bob","cwd":"/work/bob","state":"active","updatedAt":"` + v2TestStamp + `"}]}`
	fields := ImportSeatFields{Tab: "Build", Profile: "worker", Role: "worker", Workspace: "workspace", Persona: "alice", Key: KeyRequest{Routing: map[Provider]any{}}}
	list, err := ImportSeatListV1(legacy, ImportInputs{Seats: map[string]ImportSeatFields{"alice": fields, "bob": fields}, Policy: map[string]RolePolicy{"worker": {PolicyID: "policy"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if list.Seats[0].Parent == nil || *list.Seats[0].Parent != "bob" {
		t.Fatal(list.Seats[0])
	}
}

func TestV2ImportMapAndExplicitLegacyMapping(t *testing.T) {
	policyBytes, err := os.ReadFile("testdata/policy-table.json")
	if err != nil {
		t.Fatal(err)
	}
	policies, err := ParsePolicyTable(string(policyBytes))
	if err != nil {
		t.Fatal(err)
	}
	legacy := `{"version":1,"seats":[{"name":"lead","cwd":"/work/lead","state":"dormant","updatedAt":"2026-11-07T00:00:00.000Z","freshAfter":"2026-11-07T01:00:00.000Z","note":"keep this note"}]}`
	mapJSON := `{"lead":{"tab":"Build","profile":"build-profile","role":"build.coordination.lead","roleTreeRevision":7,"workspace":"workspace-main","persona":"lead-persona","key":{"routing":{"claude":["team-build"]},"limits":{"keyCap":{"usd":10,"window":{"kind":"rolling","seconds":86400}}}}}}`
	importMap, err := ParseImportMap(mapJSON)
	if err != nil {
		t.Fatal(err)
	}
	fields := importMap["lead"]
	list, err := ImportSeatListV1(legacy, ImportInputs{Seats: importMap, Policy: policies}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertImportedSeat(t, list)
	for _, invalid := range []struct {
		name   string
		inputs ImportInputs
		want   string
	}{
		{name: "extra seat map row", inputs: ImportInputs{Seats: map[string]ImportSeatFields{"lead": fields, "extra": fields}, Policy: policies}, want: "import map has unknown seat extra"},
		{name: "unknown role", inputs: ImportInputs{Seats: map[string]ImportSeatFields{"lead": func() ImportSeatFields { value := fields; value.Role = "unknown.role"; return value }()}, Policy: policies}, want: "role unknown.role has no policy"},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			_, err := ImportSeatListV1(legacy, invalid.inputs, nil)
			if err == nil || !strings.Contains(err.Error(), invalid.want) {
				t.Fatalf("error=%v want %q", err, invalid.want)
			}
		})
	}
	if _, err := ParseImportMap(`{"lead":{"tab":"Build"}}`); err == nil || !strings.Contains(err.Error(), "profile must be a non-empty string") {
		t.Fatalf("missing field error=%v", err)
	}
	if _, err := ParseImportMap(`{"lead":{"tab":"Build","surprise":true}}`); err == nil || !strings.Contains(err.Error(), `unknown key "surprise"`) {
		t.Fatalf("unknown map field error=%v", err)
	}
}

// v1 seats absent from the import map are skipped and stay in the v1 backup.
func TestV1ImportSkipsSeatAbsentFromMap(t *testing.T) {
	policyBytes, err := os.ReadFile("testdata/policy-table.json")
	if err != nil {
		t.Fatal(err)
	}
	policies, err := ParsePolicyTable(string(policyBytes))
	if err != nil {
		t.Fatal(err)
	}
	legacy := `{"version":1,"seats":[{"name":"lead","cwd":"/work/lead","state":"active","updatedAt":"2026-11-07T00:00:00.000Z"},{"name":"ghost","cwd":"/work/ghost","state":"active","updatedAt":"2026-11-07T00:00:00.000Z"}]}`
	mapJSON := `{"lead":{"tab":"Build","profile":"build-profile","role":"build.coordination.lead","roleTreeRevision":7,"workspace":"workspace-main","persona":"lead-persona","key":{"routing":{},"limits":{}}}}`
	importMap, err := ParseImportMap(mapJSON)
	if err != nil {
		t.Fatal(err)
	}
	list, err := ImportSeatListV1(legacy, ImportInputs{Seats: importMap, Policy: policies}, nil)
	if err != nil {
		t.Fatalf("absent v1 seat refused the import: %v", err)
	}
	if len(list.Seats) != 1 || list.Seats[0].Name != "lead" {
		t.Fatalf("imported seats = %#v, want only lead", list.Seats)
	}
}

// A map entry with no v1 seat is still refused.
func TestV1ImportRefusesMapEntryAbsentFromV1(t *testing.T) {
	policyBytes, err := os.ReadFile("testdata/policy-table.json")
	if err != nil {
		t.Fatal(err)
	}
	policies, err := ParsePolicyTable(string(policyBytes))
	if err != nil {
		t.Fatal(err)
	}
	legacy := `{"version":1,"seats":[{"name":"lead","cwd":"/work/lead","state":"active","updatedAt":"2026-11-07T00:00:00.000Z"}]}`
	mapJSON := `{"lead":{"tab":"Build","profile":"build-profile","role":"build.coordination.lead","roleTreeRevision":7,"workspace":"workspace-main","persona":"lead-persona","key":{"routing":{},"limits":{}}},"extra":{"tab":"Build","profile":"build-profile","role":"build.coordination.lead","roleTreeRevision":7,"workspace":"workspace-main","persona":"lead-persona","key":{"routing":{},"limits":{}}}}`
	importMap, err := ParseImportMap(mapJSON)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ImportSeatListV1(legacy, ImportInputs{Seats: importMap, Policy: policies}, nil)
	if err == nil || !strings.Contains(err.Error(), "import map has unknown seat extra") {
		t.Fatalf("error=%v, want unknown map seat refusal", err)
	}
}
func TestImportMapParsesSharedProviderRouting(t *testing.T) {
	const text = `{"seat":{"tab":"infra","profile":"default","role":"build.coordination.lead","roleTreeRevision":1,"workspace":"seat","persona":"seat","key":{"routing":{"claude":"shared","codex":"shared"},"limits":{}}}}`
	seats, err := ParseImportMap(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(seats) != 1 || seats["seat"].Key.Routing[ProviderClaude] != sharedRouting || seats["seat"].Key.Routing[ProviderCodex] != sharedRouting {
		t.Fatalf("shared provider routing = %#v", seats)
	}
}
func TestV1ImportOmitsRetiredSeatWithoutMapEntry(t *testing.T) {
	legacy := `{"version":1,"seats":[{"name":"kept","cwd":"/work/kept","state":"active","updatedAt":"` + v2TestStamp + `"},{"name":"retired","cwd":"/work/retired","state":"retired","updatedAt":"` + v2TestStamp + `"}]}`
	fields := ImportSeatFields{Tab: "Build", Profile: "worker", Role: "worker", RoleTreeRevision: 1, Workspace: "kept", Persona: "kept", Key: KeyRequest{Routing: map[Provider]any{}}}
	list, err := ImportSeatListV1(legacy, ImportInputs{Seats: map[string]ImportSeatFields{"kept": fields}, Policy: map[string]RolePolicy{"worker": {PolicyID: "policy"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Seats) != 1 || list.Seats[0].Name != "kept" {
		t.Fatalf("imported seats = %#v, want only kept", list.Seats)
	}
}

func assertImportedSeat(t *testing.T, list SeatListV2) {
	t.Helper()
	seat := list.Seats[0]
	if list.Version != 2 || list.Revision != 1 || seat.State != SeatV2Paused || seat.Tab != "Build" || seat.Profile != "build-profile" || seat.RoleTreeRevision != 7 || seat.Workspace != "workspace-main" || seat.Persona != "lead-persona" || seat.Key.PolicyID != "agents-build-v1" || seat.Note == nil || *seat.Note != "keep this note" {
		t.Fatalf("imported list = %#v", list)
	}
	if seat.Name != "lead" || seat.SeatRevision != 1 || seat.CWD != "/work/lead" || seat.RestartGeneration != 0 || seat.UpdatedAt != "2026-11-07T00:00:00.000Z" || seat.Role != "build.coordination.lead" {
		t.Fatalf("imported seat fields = %#v", seat)
	}
	if selected, ok := seat.Key.Routing[ProviderClaude].([]string); !ok || len(selected) != 1 || selected[0] != "team-build" {
		t.Fatalf("imported routing = %#v", seat.Key.Routing)
	}
	if cap := seat.Key.Limits.KeyCap; cap == nil || cap.USD != 10 || cap.Window != (Window{Kind: "rolling", Seconds: 86400}) || seat.Key.Limits.PerModel != nil {
		t.Fatalf("imported limits = %#v", seat.Key.Limits)
	}
}
