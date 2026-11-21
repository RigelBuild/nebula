// SPDX-License-Identifier: MIT OR Apache-2.0

package waveseats

import "testing"

func TestParsePersona(t *testing.T) {
	t.Run("reads name role and tab", func(t *testing.T) {
		got, err := ParsePersona("---\nname: web\nrole: worker\nzellij:\n  tab: products\n---\nbody\n")
		if err != nil || got.Name != "web" || got.Role == nil || *got.Role != "worker" || got.Tab == nil || *got.Tab != "products" {
			t.Fatalf("ParsePersona = %#v, %v", got, err)
		}
	})
	t.Run("optional fields absent", func(t *testing.T) {
		got, err := ParsePersona("---\nname: worker\n---\n")
		if err != nil || got.Name != "worker" || got.Role != nil || got.Tab != nil {
			t.Fatalf("ParsePersona = %#v, %v", got, err)
		}
	})
	t.Run("invalid frontmatter", func(t *testing.T) {
		for _, test := range []struct{ source, message string }{
			{"body", "persona has no YAML frontmatter"},
			{"---\n[]\n---\n", "persona frontmatter must be a mapping"},
			{"---\nrole: worker\n---\n", "persona frontmatter must have a name"},
		} {
			if _, err := ParsePersona(test.source); err == nil || err.Error() != test.message {
				t.Errorf("ParsePersona(%q) error = %v, want %q", test.source, err, test.message)
			}
		}
	})
	t.Run("CRLF and blank tab", func(t *testing.T) {
		got, err := ParsePersona("---\r\nname: worker\r\nzellij:\r\n  tab: ' '\r\n---\r\n")
		if err != nil || got.Tab != nil {
			t.Fatalf("ParsePersona = %#v, %v", got, err)
		}
	})
}
