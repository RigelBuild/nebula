package issueowner

import (
	"reflect"
	"testing"
)

func TestReservedCodenames(t *testing.T) {
	want := []string{"mintaka", "seal", "renovate", "dependabot", "trunk-merge", "graphite-base"}
	if !reflect.DeepEqual(ReservedCodenames, want) {
		t.Fatalf("ReservedCodenames = %v, want %v", ReservedCodenames, want)
	}
}

func TestIsReservedCodename(t *testing.T) {
	tests := []struct {
		name     string
		codename string
		expected bool
	}{
		{name: "known codename", codename: "mintaka", expected: true},
		{name: "case insensitive", codename: "Graphite-Base", expected: true},
		{name: "ordinary codename", codename: "ci-build", expected: false},
		{name: "empty codename", codename: "", expected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsReservedCodename(tt.codename); got != tt.expected {
				t.Fatalf("IsReservedCodename(%q) = %t, want %t", tt.codename, got, tt.expected)
			}
		})
	}
}
