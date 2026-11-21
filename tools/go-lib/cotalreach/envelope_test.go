package cotalreach

import "testing"

func TestPsEnvelope(t *testing.T) {
	cases := []struct {
		name, input string
		valid       bool
		present     string
	}{
		{name: "seats", input: `{"schemaVersion":1,"command":"ps","outcome":"ok","result":{"kind":"seats","items":[{"name":"a","mesh":"working"}]}}`, valid: true, present: "a"},
		{name: "partial presence", input: `{"schemaVersion":1,"command":"ps","outcome":"partial","result":{"kind":"presence","items":[{"name":"b","status":"idle"}]}}`, valid: true, present: "b"},
		{name: "invalid outcome", input: `{"schemaVersion":1,"command":"ps","outcome":"error","result":{"kind":"presence","items":[]}}`},
		{name: "missing items", input: `{"schemaVersion":1,"command":"ps","outcome":"ok","result":{"kind":"seats"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, valid := ParsePsItems(tc.input)
			if valid != tc.valid {
				t.Fatalf("valid=%v", valid)
			}
			if tc.present != "" {
				if _, ok := ParsePresentAgentNames(tc.input)[tc.present]; !ok {
					t.Fatalf("missing %s", tc.present)
				}
			}
		})
	}
}
