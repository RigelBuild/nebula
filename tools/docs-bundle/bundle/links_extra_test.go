// SPDX-License-Identifier: MIT OR Apache-2.0

package bundle

import (
	"reflect"
	"testing"
)

func TestLinkTargetsReferenceCases(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []linkTarget
	}{
		{"reference apostrophe", "[k]: docs/don't.md\n", []linkTarget{{start: 5, end: 18, raw: "docs/don't.md"}}},
		{"reference markdown", "[ref]: next.md\n```\n[hidden]: missing.md\n```\n", []linkTarget{{start: 7, end: 14, raw: "next.md"}}},
		{"footnote", "[^1]: This is a footnote.\n", []linkTarget{}},
		{"quoted statement", "[rule]: \"A quoted statement.\"\n", []linkTarget{}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := linkTargets(testCase.input); !reflect.DeepEqual(got, testCase.want) {
				t.Errorf("linkTargets() = %#v, want %#v", got, testCase.want)
			}
		})
	}
}
