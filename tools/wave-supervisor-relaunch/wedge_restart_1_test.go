package main

import (
	"context"
	"testing"
)

func TestCheckWedge(t *testing.T) {
	tests := []struct {
		name     string
		probes   []bool
		identity []string
		want     WedgeVerdict
	}{
		{name: "a good first probe is healthy", probes: []bool{true}, identity: []string{"500:1"}, want: WedgeVerdict{Kind: "healthy"}},
		{name: "two good probes are healthy", probes: []bool{false, true, true}, identity: []string{"500:1", "500:1", "500:1"}, want: WedgeVerdict{Kind: "healthy"}},
		{name: "three failed probes on one identity are wedged", probes: []bool{false, false, false}, identity: []string{"500:1", "500:1", "500:1"}, want: WedgeVerdict{Kind: "wedged", Identity: "500:1"}},
		{name: "changing identity makes failures unverified", probes: []bool{false, false, false}, identity: []string{"500:1", "500:1", "501:2"}, want: WedgeVerdict{Kind: "unverified"}},
		{name: "no server identity reports down", probes: []bool{false, false, false}, identity: []string{"none", "none", "none"}, want: WedgeVerdict{Kind: "down"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probeIndex, identityIndex := 0, 0
			got := checkWedge(t.Context(), WedgeDeps{
				Probe: func() (bool, error) {
					if probeIndex >= len(tt.probes) {
						return false, nil
					}
					value := tt.probes[probeIndex]
					probeIndex++
					return value, nil
				},
				Identity: func(context.Context) string {
					if identityIndex >= len(tt.identity) {
						return tt.identity[len(tt.identity)-1]
					}
					value := tt.identity[identityIndex]
					identityIndex++
					return value
				},
				Sleep: func(int64) error { return nil },
			})
			if got != tt.want {
				t.Fatalf("checkWedge() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
