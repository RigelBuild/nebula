// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import (
	"maps"
	"slices"
	"testing"
)

const presencePS = `{"schemaVersion":1,"command":"ps","outcome":"ok","result":{"kind":"seats","items":[{"name":"alice"}]}}`

func TestPresentNames(t *testing.T) {
	ready := map[string]any{"state": "idle", "lastSweep": map[string]any{"completedAt": "t"}}
	failing := func(field, value string) map[string]any {
		return map[string]any{"staticReconciliation": map[string]any{"state": "idle", "failures": []any{map[string]any{"alias": "bob", field: value}}}}
	}
	for _, tc := range []struct {
		name    string
		ps      string
		holders string
		status  any
		want    []string
	}{
		{name: "ps row", ps: presencePS, status: ready, want: []string{"alice"}},
		{name: "running holder", holders: `{"name":"bob","status":"running"}` + "\n" + `{"name":"carol","status":"exited"}`, status: ready, want: []string{"bob"}},
		{name: "retrying phase", status: failing("phase", "retrying"), want: []string{"bob"}},
		{name: "retry-scheduled disposition", status: failing("disposition", "retry-scheduled"), want: []string{"bob"}},
		{name: "terminal failure is absent", status: failing("phase", "failed"), want: []string{}},
		{name: "nothing", status: nil, want: []string{}},
		{name: "manager-wide waiting status marks no seat present", status: map[string]any{"state": "running", "lastSweep": map[string]any{"completedAt": "done"}}, want: []string{}},
		{name: "retrying disposition with another phase", status: map[string]any{"staticReconciliation": map[string]any{"state": "idle", "failures": []any{map[string]any{"alias": "alice", "phase": "terminalizing", "disposition": "retrying"}}}}, want: []string{"alice"}},
		{name: "invalid ps envelope adds nothing", ps: "{\"name\":\"alice\"}\n", status: ready, want: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := slices.Sorted(maps.Keys(PresentNames(tc.ps, tc.holders, tc.status, []string{"alice", "bob"})))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("PresentNames = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestManagerStatusAvailable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status any
		want   bool
	}{
		{name: "flat", status: map[string]any{"state": "idle", "lastSweep": map[string]any{"completedAt": "t"}}, want: true},
		{name: "nested", status: map[string]any{"staticReconciliation": map[string]any{"state": "idle", "lastSweep": map[string]any{"completedAt": "t"}}}, want: true},
		{name: "no sweep", status: map[string]any{"state": "idle"}},
		{name: "empty completedAt", status: map[string]any{"state": "idle", "lastSweep": map[string]any{"completedAt": ""}}},
		{name: "no state", status: map[string]any{"lastSweep": map[string]any{"completedAt": "t"}}},
		{name: "not a map", status: "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ManagerStatusAvailable(tc.status); got != tc.want {
				t.Fatalf("ManagerStatusAvailable = %v, want %v", got, tc.want)
			}
		})
	}
}
